package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

// const workDir = "/tmp/iden3-CIT"
// const confDir = "/home/dev/git/iden3/continuous-integration-testing/tmp"
// const port = "8010"
// const loopTimeout = 60
// const testTimeout = 120
// const setupTimeout = 120

type Config struct {
	WorkDir  string
	ConfDir  string `mapstructure:"-"`
	Listen   string
	Timeouts struct {
		Loop  int64
		Test  int64
		Setup int64
	}
}

type Timeouts struct {
	Loop  time.Duration
	Test  time.Duration
	Setup time.Duration
}

type Script struct {
	Path string
	Cmd  *exec.Cmd
}

func (s *Script) Run(ctx context.Context, preludePath, runPath, outDir string) {
	if err := os.MkdirAll(outDir, 0700); err != nil {
		log.Fatal(err)
	}
	log.Debugf("Running %s", s.Path)
	s.Cmd = exec.CommandContext(ctx, preludePath, s.Path)
	s.Cmd.Dir = runPath
	s.Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := s.Cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	stderr, err := s.Cmd.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Cmd.Start(); err != nil {
		log.Fatal(err)
	}
	outFileName := fmt.Sprintf("%s.out.txt", path.Base(s.Path))
	outputHandler(stdout, stderr, path.Join(outDir, outFileName), "", nil)
	// stdoutFileName := fmt.Sprintf("%s.stdout.txt", path.Base(s.Path))
	// go writeOutFile(stdout, path.Join(outDir, stdoutFileName))
	// stderrFileName := fmt.Sprintf("%s.stderr.txt", path.Base(s.Path))
	// go writeOutFile(stderr, path.Join(outDir, stderrFileName))
}

func (s *Script) Wait() error {
	err := s.Cmd.Wait()
	log.Debugf("Finished %s with err: %v", s.Path, err)
	return err
}

func (s *Script) Stop() (*os.ProcessState, error) {
	if s.Cmd == nil || s.Cmd.Process == nil {
		return nil, fmt.Errorf("Script Cmd or Cmd.Process is nil")
	}
	defer func() { s.Cmd = nil }()
	pgid, err := syscall.Getpgid(s.Cmd.Process.Pid)
	if err != nil {
		return nil, fmt.Errorf("Unable to get script process pgid")
	}
	if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil {
		return nil, err
	}
	wait := make(chan struct {
		ps  *os.ProcessState
		err error
	})
	go func() {
		ps, err := s.Cmd.Process.Wait()
		wait <- struct {
			ps  *os.ProcessState
			err error
		}{ps, err}
	}()
	select {
	case res := <-wait:
		if res.err != nil {
			return nil, res.err
		}
		return res.ps, nil
	case <-time.After(30 * time.Second):
		syscall.Kill(-pgid, syscall.SIGKILL)
		return nil, fmt.Errorf("Script took more than 30 seconds to terminate, killed")
	}
}

type Scripts struct {
	Setup       *Script
	Start       []*Script
	Test        []*Script
	Stop        *Script
	PreludePath string
}

type Repo struct {
	GitRepo *git.Repository
	URL     string
	Scripts Scripts
	OutDir  string
	Dir     string
}

// func (r *Repo) URL() (string, error) {
// 	remotes, err := r.GitRepo.Remotes()
// 	if err != nil {
// 		return "", err
// 	}
// 	return remotes[0].Config().URLs[0], nil
// }

func (r *Repo) HeadHash() (plumbing.Hash, error) {
	ref, err := r.GitRepo.Head()
	if err != nil {
		return plumbing.Hash{}, err
	}
	return ref.Hash(), nil
}

func (r *Repo) Name() string {
	return strings.TrimSuffix(r.URL[strings.LastIndex(r.URL, "/")+1:], ".git")
}

type Repos struct {
	Repos    []*Repo
	Dir      string
	ConfDir  string
	OutDir   string
	Scripts  Scripts
	Timeouts Timeouts
}

func NewRepos(confDir, outDir, reposDir string, reposUrl []string,
	timeouts Timeouts) (*Repos, error) {
	repos := []*Repo{}
	for _, repoUrl := range reposUrl {
		name := repoUrl[strings.LastIndex(repoUrl, "/")+1:]
		repoDir := path.Join(reposDir, name)
		log.Infof("Cloning %s into %s", repoUrl, repoDir)
		gitRepo, err := git.PlainClone(repoDir, false, &git.CloneOptions{
			SingleBranch: true,
			URL:          repoUrl,
			Progress:     os.Stdout,
		})
		if err == git.ErrRepositoryAlreadyExists {
			log.Infof("Repository %s already exists", repoUrl)
			gitRepo, err = git.PlainOpen(repoDir)
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		repo := Repo{GitRepo: gitRepo, URL: repoUrl, Dir: repoDir}

		repoConfDir := path.Join(confDir, repo.Name())
		files, err := ioutil.ReadDir(repoConfDir)
		if err != nil {
			log.Warn("DBG conf dir not found, skipping...")
			continue
			// return nil, err
		}
		var scripts Scripts
		for _, file := range files {
			filePath := path.Join(repoConfDir, file.Name())
			if strings.HasPrefix(file.Name(), "setup") {
				scripts.Setup = &Script{Path: filePath}
			} else if strings.HasPrefix(file.Name(), "start") {
				scripts.Start = append(scripts.Start, &Script{Path: filePath})
			} else if strings.HasPrefix(file.Name(), "test") {
				scripts.Test = append(scripts.Test, &Script{Path: filePath})
			} else if strings.HasPrefix(file.Name(), "stop") {
				scripts.Stop = &Script{Path: filePath}
			}
		}
		scripts.PreludePath = path.Join(confDir, "prelude")
		log.Infof("Loaded repository scripts at %v", repoConfDir)
		repo.Scripts = scripts
		repo.OutDir = path.Join(outDir, repo.Name())
		repos = append(repos, &repo)
	}
	return &Repos{
		Repos:    repos,
		Dir:      reposDir,
		ConfDir:  confDir,
		OutDir:   outDir,
		Timeouts: timeouts,
		Scripts: Scripts{
			Setup:       &Script{Path: path.Join(confDir, "setup")},
			Stop:        &Script{Path: path.Join(confDir, "stop")},
			PreludePath: path.Join(confDir, "prelude"),
		},
	}, nil
}

func (rs *Repos) Update() (bool, error) {
	updated := false
	for _, repo := range rs.Repos {
		repoUrl := repo.URL
		log.Infof("Updating %s", repoUrl)
		wt, err := repo.GitRepo.Worktree()
		if err != nil {
			return false, err
		}
		if err := wt.Pull(&git.PullOptions{
			SingleBranch: true,
			Progress:     os.Stdout,
			Force:        true,
		}); err == git.NoErrAlreadyUpToDate {
			log.Infof("Repository %s already up-to-date", repoUrl)
		} else if err != nil {
			return false, err
		} else {
			updated = true
		}
		head, err := repo.HeadHash()
		if err != nil {
			return false, err
		}
		log.Infof("Repository %s at commit %s", repoUrl, head)
	}
	return updated, nil
}

func writeOutFile(out io.ReadCloser, filePath string) {
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0650)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, out); err != nil {
		pathErr, ok := err.(*os.PathError)
		if !(ok && pathErr.Op == "read") {
			log.Fatalf("%#v", err)
		}
	}
}

func outputHandler(stdout io.ReadCloser, stderr io.ReadCloser, filePath string,
	pattern string, readyCh chan bool) {
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0650)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	lineCh := make(chan string)
	endCh := make(chan error)
	readLine := func(rd io.Reader) {
		reader := bufio.NewReader(rd)
		for {
			line, err := reader.ReadString('\n')
			lineCh <- line
			if err != nil {
				// log.Debugf("DBG readLine %v", err)
				break
			}
		}
		endCh <- err
	}
	go readLine(stdout)
	go readLine(stderr)
	ready := false
	if pattern == "" {
		ready = true
	}
	for {
		select {
		case line := <-lineCh:
			if !ready && strings.Contains(line, pattern) {
				readyCh <- true
				ready = true
			}
			// log.Debugf("DBG %v: %v", filePath, line)
			_, err := io.WriteString(f, line)
			if err != nil {
				log.Fatalf("Can't write output to file %v: %v", filePath, err)
			}
		case err := <-endCh:
			if err != nil && err != io.EOF {
				log.Errorf("Process stdout/stderr error: %v", err)
			}
			return
		}
	}
}

func (rs *Repos) Run() {
	now := time.Now()
	ts := fmt.Sprintf("%08d_%s", now.Unix(), now.Format(time.RFC3339))
	outDir := path.Join(rs.OutDir, ts)

	// Setup
	log.Infof("--- Setup ---")
	ctxSetup, cancelSetup := context.WithTimeout(context.Background(),
		rs.Timeouts.Setup*time.Second)
	defer cancelSetup()
	rs.Scripts.Setup.Run(ctxSetup, rs.Scripts.PreludePath, "", outDir)
	if err := rs.Scripts.Setup.Wait(); err != nil {
		log.Errorf("Scripts.Setup error: %v", err)
	}
	for _, repo := range rs.Repos {
		script := repo.Scripts.Setup
		ctx, cancel := context.WithTimeout(context.Background(),
			rs.Timeouts.Setup*time.Second)
		defer cancel()
		script.Run(ctx, rs.Scripts.PreludePath, repo.Dir, path.Join(outDir, repo.Name()))
		if err := script.Wait(); err != nil {
			log.Errorf("Setup %v -> %v finished with error: %v",
				repo.Name(), script.Path, err)
		}
	}

	// Start
	log.Infof("--- Start ---")
	ctxStart, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Start {
			ctx, cancel := context.WithCancel(ctxStart)
			defer cancel()
			s := script
			repoName := repo.Name()
			go func() {
				s.Run(ctx, rs.Scripts.PreludePath, repo.Dir,
					path.Join(outDir, repoName))
			}()
		}
	}

	// Test
	log.Infof("--- Test ---")
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Test {
			ctx, cancel := context.WithTimeout(context.Background(),
				rs.Timeouts.Test*time.Second)
			defer cancel()
			script.Run(ctx, rs.Scripts.PreludePath, repo.Dir,
				path.Join(outDir, repo.Name()))
			if err := script.Wait(); err != nil {
				log.Errorf("Test %v -> %v finished with error: %v",
					repo.Name(), script.Path, err)
			}
		}
	}

	// Stop
	time.Sleep(10 * time.Second)
	log.Infof("--- Stop ---")
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Start {
			_, err := script.Stop()
			if err != nil {
				log.Errorf("Error stopping Start %v -> %v: %v", repo.Name(),
					script.Path, err)
			}
		}
	}
}

func (rs *Repos) UpdateLoop() {
	rs.Run()
	for {
		time.Sleep(rs.Timeouts.Loop * time.Second)
		updated, err := rs.Update()
		if err != nil {
			log.Fatal(err)
		}
		if updated {
			rs.Run()
		}
	}
}

func printUsage() {
	fmt.Printf("Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	confDir := flag.String("conf", "", "config directory")
	debug := flag.Bool("debug", false, "enable debug output")
	quiet := flag.Bool("quiet", false, "output warnings and errors only")
	flag.Parse()
	if *confDir == "" {
		printUsage()
		return
	}
	if *debug {
		log.SetLevel(log.DebugLevel)
	} else if *quiet {
		log.SetLevel(log.WarnLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	if !path.IsAbs(*confDir) {
		*confDir = path.Join(wd, *confDir)
	}
	viper.SetConfigType("toml")
	viper.SetConfigName("config")
	viper.AddConfigPath(*confDir)

	if err := viper.ReadInConfig(); err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatal(err)
	}
	cfg.ConfDir = *confDir
	fmt.Printf("%#v\n", cfg)

	reposUrl := []string{
		"https://github.com/iden3/go-iden3.git",
		"https://github.com/iden3/iden3js.git",
		"https://github.com/iden3/notifications-server.git",
		"https://github.com/iden3/tx-forwarder.git",
	}
	if err := os.MkdirAll(cfg.WorkDir, 0700); err != nil {
		log.Fatal(err)
	}
	reposDir := path.Join(cfg.WorkDir, "git")
	if err := os.MkdirAll(reposDir, 0700); err != nil {
		log.Fatal(err)
	}
	outDir := path.Join(cfg.WorkDir, "out")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		log.Fatal(err)
	}

	repos, err := NewRepos(cfg.ConfDir, outDir, reposDir, reposUrl,
		Timeouts{
			Loop:  time.Duration(cfg.Timeouts.Loop),
			Setup: time.Duration(cfg.Timeouts.Setup),
			Test:  time.Duration(cfg.Timeouts.Test),
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	go repos.UpdateLoop()

	http.Handle("/", http.FileServer(http.Dir(cfg.WorkDir)))

	log.Infof("Serving %s on http://%s", cfg.WorkDir, cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}
