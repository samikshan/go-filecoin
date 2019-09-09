package main

import (
	"context"
	"crypto/rand"
	flg "flag"
	"fmt"
	"github.com/filecoin-project/go-filecoin/types"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/ipfs/go-ipfs-files"
	logging "github.com/ipfs/go-log"
	"github.com/mitchellh/go-homedir"

	"github.com/filecoin-project/go-filecoin/porcelain"
	"github.com/filecoin-project/go-filecoin/protocol/storage/storagedeal"
	"github.com/filecoin-project/go-filecoin/tools/fast"
	"github.com/filecoin-project/go-filecoin/tools/fast/environment"
	"github.com/filecoin-project/go-filecoin/tools/fast/series"
	lpfc "github.com/filecoin-project/go-filecoin/tools/iptb-plugins/filecoin/local"
)

var (
	network string = "user"
	workdir string
	binpath string
	err     error

	exitcode int

	flag = flg.NewFlagSet(os.Args[0], flg.ExitOnError)
)

func init() {
	logging.SetDebugLogging()

	var (
		err error
	)

	// We default to the binary built in the project directory, fallback
	// to searching path.
	binpath, err = getFilecoinBinary()
	if err != nil {
		// Look for `go-filecoin` in the path to set `binpath` default
		// If the binary is not found, an error will be returned. If the
		// error is ErrNotFound we ignore it.
		// Error is handled after flag parsing so help can be shown without
		// erroring first
		binpath, err = exec.LookPath("go-filecoin")
		if err != nil {
			xerr, ok := err.(*exec.Error)
			if ok && xerr.Err == exec.ErrNotFound {
				err = nil
			}
		}
	}

	flag.StringVar(&network, "network", network, "set the network name to run against")
	flag.StringVar(&workdir, "workdir", workdir, "set the working directory used to store filecoin repos")
	flag.StringVar(&binpath, "binpath", binpath, "set the binary used when executing `go-filecoin` commands")

	// ExitOnError is set
	flag.Parse(os.Args[1:]) // nolint: errcheck

	// If we failed to find `go-filecoin` and it was not set, handle the error
	if len(binpath) == 0 {
		msg := "failed when checking for `go-filecoin` binary;"
		if err == nil {
			err = fmt.Errorf("no binary provided or found")
			msg = "please install or build `go-filecoin`;"
		}

		handleError(err, msg)
		os.Exit(1)
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	exit := make(chan struct{}, 1)

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		<-signals
		fmt.Println("Ctrl-C received, starting shutdown")
		cancel()
		exit <- struct{}{}
	}()

	defer func() {
		if r := recover(); r != nil {
			fmt.Println("recovered from panic", r)
			fmt.Println("stacktrace from panic: \n" + string(debug.Stack()))
			exitcode = 1
		}
		os.Exit(exitcode)
	}()

	if len(workdir) == 0 {
		workdir, err = ioutil.TempDir("", "deal-maker")
		if err != nil {
			exitcode = handleError(err)
			return
		}
	}

	if ok, err := isEmpty(workdir); !ok {
		if err == nil {
			err = fmt.Errorf("workdir is not empty: %s", workdir)
		}

		exitcode = handleError(err, "fail when checking workdir;")
		return
	}

	env, err := environment.NewDevnet(network, workdir)
	if err != nil {
		exitcode = handleError(err)
		return
	}

	// Defer the teardown, this will shuteverything down for us
	defer env.Teardown(ctx) // nolint: errcheck

	// Setup localfilecoin plugin options
	options := make(map[string]string)
	options[lpfc.AttrLogJSON] = "0"            // Disable JSON logs
	options[lpfc.AttrLogLevel] = "4"           // Set log level to Info
	options[lpfc.AttrFilecoinBinary] = binpath // Use the repo binary

	genesisURI := env.GenesisCar()

	fastenvOpts := fast.FilecoinOpts{
		InitOpts:   []fast.ProcessInitOption{fast.PODevnet(network), fast.POGenesisFile(genesisURI)},
		DaemonOpts: []fast.ProcessDaemonOption{},
	}

	// The genesis process is the filecoin node that loads the miner that is
	// define with power in the genesis block, and the prefunnded wallet
	node, err := env.NewProcess(ctx, lpfc.PluginName, options, fastenvOpts)
	if err != nil {
		exitcode = handleError(err, "failed to create genesis process;")
		return
	}

	err = series.InitAndStart(ctx, node)
	if err != nil {
		exitcode = handleError(err, "failed series.InitAndStart;")
		return
	}

	err = env.GetFunds(ctx, node)
	if err != nil {
		exitcode = handleError(err, "failed env.GetFunds;")
		return
	}

	pparams, err := node.Protocol(ctx)
	if err != nil {
		exitcode = handleError(err, "failed node.Protocol;")
		return
	}

	sinfo := pparams.SupportedSectors[0]

	validMiners := make(map[string]struct{})
	for _, miner := range flag.Args() {
		validMiners[miner] = struct{}{}
	}

	for {
		dec, err := node.ClientListAsks(ctx)
		if err != nil {
			fmt.Printf("ERROR: failed to list asks\n")
			continue
		}

		asks := make(map[string]porcelain.Ask)
		for {
			var ask porcelain.Ask

			err := dec.Decode(&ask)
			if err != nil && err != io.EOF {
				fmt.Printf("ERROR: %s\n", err)
				continue
			}

			if err == io.EOF {
				break
			}

			askMiner := ask.Miner.String()

			if _, ok := validMiners[askMiner]; ok {
				// Is a valid miner to make a deal with

				if a, ok := asks[askMiner]; ok && a.ID > ask.ID {
					continue
				}

				asks[askMiner] = ask
			}
		}

		if len(asks) == 0 {
			time.Sleep(time.Minute)
		}

		for _, ask := range asks {
			dataReader := io.LimitReader(rand.Reader, int64(sinfo.MaxPieceSize.Uint64()))
			_, deal, err := series.ImportAndStoreWithDuration(ctx, node, ask, 256, files.NewReaderFile(dataReader))
			if err != nil {
				fmt.Printf("ERROR: %s\n", err)
				continue
			}

			_, err = series.WaitForDealState(ctx, node, deal, storagedeal.Complete)
			if err != nil {
				fmt.Printf("ERROR: %s\n", err)
				continue
			}
		}
	}

	<-exit
}

func handleError(err error, msg ...string) int {
	if err == nil {
		return 0
	}

	if len(msg) != 0 {
		fmt.Println(msg[0], err)
	} else {
		fmt.Println(err)
	}

	return 1
}

// https://stackoverflow.com/a/30708914
func isEmpty(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close() // nolint: errcheck

	_, err = f.Readdirnames(1) // Or f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err // Either not empty or error, suits both cases
}

func getProofsMode(smallSectors bool) types.ProofsMode {
	if smallSectors {
		return types.TestProofsMode
	}
	return types.LiveProofsMode
}

func getFilecoinBinary() (string, error) {
	gopath, err := getGoPath()
	if err != nil {
		return "", err
	}

	bin := filepath.Join(gopath, "/src/github.com/filecoin-project/go-filecoin/go-filecoin")
	_, err = os.Stat(bin)
	if err != nil {
		return "", err
	}

	if os.IsNotExist(err) {
		return "", err
	}

	return bin, nil
}

func getGoPath() (string, error) {
	gp := os.Getenv("GOPATH")
	if gp != "" {
		return gp, nil
	}

	home, err := homedir.Dir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, "go"), nil
}

/*
 */