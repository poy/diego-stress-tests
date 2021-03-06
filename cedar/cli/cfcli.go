package cli

import (
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/glycerine/rbuf"

	"code.cloudfoundry.org/cflager"
	"code.cloudfoundry.org/lager"
	"golang.org/x/net/context"
)

//go:generate counterfeiter -o fakes/fake_cfclient.go . CFClient
type CFClient interface {
	Cf(logger lager.Logger, ctx context.Context, timeout time.Duration, args ...string) ([]byte, error)
	Cleanup(ctx context.Context)
	Pool() chan string
}

type CFPooledClient struct {
	poolSize int
	pool     chan string
	homeDir  string
}

func NewCfClient(ctx context.Context, poolSize int) CFClient {
	logger, ok := ctx.Value("logger").(lager.Logger)
	if !ok {
		logger, _ = cflager.New("cedar")
	}
	logger = logger.Session("cf")
	user, err := user.Current()
	if err != nil {
		logger.Error("get-home-dir-failed", err)
	}
	homeDir := user.HomeDir

	if _, err = os.Stat(filepath.Join(homeDir, ".cf")); os.IsNotExist(err) {
		logger.Error("cf-dir-unavailable", err)
		panic("cf-dir-unavailable")
	}

	pool := make(chan string, poolSize)
	for i := 0; i < poolSize; i++ {
		cfDir, err := ioutil.TempDir("", "cfhome")
		if err != nil {
			logger.Error("init-temp-cf-dir-failed", err)
		}

		cmd := exec.Command("cp", "-r", filepath.Join(homeDir, ".cf"), filepath.Join(cfDir, ".cf"))
		err = cmd.Run()
		if err != nil {
			logger.Error("copy-cf-config-failed", err)
		}
		pool <- cfDir
	}

	return &CFPooledClient{
		homeDir:  homeDir,
		pool:     pool,
		poolSize: poolSize,
	}
}

func (cfcli *CFPooledClient) Pool() chan string {
	return cfcli.pool
}

func (cfcli *CFPooledClient) Cf(logger lager.Logger, ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	cfDir := <-cfcli.pool
	defer func() { cfcli.pool <- cfDir }()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	cmd := exec.Command("cf", args...)
	cmd.Env = append(os.Environ(), "CF_HOME="+cfDir)
	cmd.Env = append(cmd.Env, "GOMAXPROCS=4")

	logger = logger.Session("cf", lager.Data{"args": args, "cfdir": cfDir})

	buf := rbuf.NewFixedSizeRingBuf(10240)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Error("failed-getting-command-stdout", err)
		return nil, err
	}
	cmd.Stderr = cmd.Stdout

	err = cmd.Start()
	if err != nil {
		logger.Error("failed-starting-cf-command", err)
		return nil, err
	}

	defer cancel()
	go func() {
		<-ctx.Done()
		cmd.Process.Signal(syscall.SIGQUIT)
	}()

	_, err = buf.ReadFrom(stdout)
	if err != nil {
		logger.Error("failed-reading-command-output", err)
		// we shouldn't exit yet, until we wait for the subprocess to exit
		cancel()
	}

	err = cmd.Wait()
	if err != nil {
		logger.Error("failed-running-cf-command", err, lager.Data{"stdout": string(buf.Bytes())})
		return nil, err
	}
	return buf.Bytes(), nil
}

func (cfcli *CFPooledClient) Cleanup(ctx context.Context) {
	logger, ok := ctx.Value("logger").(lager.Logger)
	if !ok {
		logger, _ = cflager.New("cedar")
	}
	logger = logger.Session("cf-cleanup")
	logger.Info("started", lager.Data{"tmp-dir-size": cfcli.poolSize})
	defer logger.Info("completed")

	for i := 0; i < cfcli.poolSize; i++ {
		cfdir := <-cfcli.pool
		err := os.RemoveAll(cfdir)
		if err != nil {
			logger.Error("failed-to-remove-tmpdir", err, lager.Data{"dir": cfdir})
		}
	}
}
