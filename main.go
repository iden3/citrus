package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os/signal"

	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	// "github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

var panicCh chan interface{}

func panicMain() {
	if x := recover(); x != nil {
		log.Debugf("Recovered from panic, terminating...")
		panicCh <- x
	}
}

type Result string

const (
	ResultUnk        Result = "UNK"
	ResultPass       Result = "PASS"
	ResultErr        Result = "ERR"
	ResultReady      Result = "READY"
	ResultReadyErr   Result = "READYERR"
	ResultRun        Result = "RUNNING"
	ResultUnfinished Result = "UNFINISHED"
)

type ScriptCfg struct {
	ReadyStr string
}

type RepoCfg struct {
	Branch string
}

type DockerCfg struct {
	Image string
	Pull  bool
	Binds map[string]string
}

type Config struct {
	WorkDir   string
	CitrusDir string `mapstructure:"-"`
	CfgDir    string `mapstructure:"-"`
	Debug     bool   `mapstructure:"-"`
	Force     bool   `mapstructure:"-"`
	Docker    bool   `mapstructure:"-"`
	Listen    string
	ReposUrl  []string `mapstructure:"repos"`
	Timeouts  struct {
		Loop  int64
		Setup int64
		Ready int64
		Test  int64
		Stop  int64
		Hook  int64
	}
	ScriptsCfg map[string]map[string]ScriptCfg `mapstructure:"script"`
	ReposCfg   map[string]RepoCfg              `mapstructure:"repo"`
	DockerCfg  DockerCfg                       `mapstructure:"docker"`
}

type Timeouts struct {
	Loop  time.Duration
	Setup time.Duration
	Ready time.Duration
	Test  time.Duration
	Stop  time.Duration
	Hook  time.Duration
}

type Script struct {
	//Path     string
	ReadyStr string
	Cmd      *exec.Cmd
	Result   Result
	waitCh   chan error
	// Running  bool
	PreludePath *string
	RunDir      *string
	outDir      *string
	CfgDir      *string
	repoName    *string
	FileName    string
}

// func (s *Script) FileName() string {
// 	return s.Path[strings.LastIndex(s.Path, "/")+1:]
// }

func (s *Script) RepoName() string {
	if s.repoName == nil {
		return ""
	}
	return *s.repoName
}

func (s *Script) Path() string {
	return path.Join(*s.CfgDir, s.RepoName(), s.FileName)
}

func (s *Script) OutDir(ts string) string {
	return path.Join(*s.outDir, ts, s.RepoName())
}

type ByFileName []*Script

func (s ByFileName) Len() int           { return len(s) }
func (s ByFileName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s ByFileName) Less(i, j int) bool { return s[i].FileName < s[j].FileName }

func (s *Script) Start(ctx context.Context, args []string,
	ts string, readyCh chan bool) {
	if err := os.MkdirAll(s.OutDir(ts), 0777); err != nil {
		log.Panic(err)
	}
	log.Debugf("Running %s", s.Path())
	s.Cmd = exec.CommandContext(ctx, *s.PreludePath, append([]string{s.Path()}, args...)...)
	s.Cmd.Dir = *s.RunDir
	s.Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := s.Cmd.StdoutPipe()
	if err != nil {
		log.Panic(err)
	}
	stderr, err := s.Cmd.StderrPipe()
	if err != nil {
		log.Panic(err)
	}
	// stdoutBuf := bufio.NewReader(stdout)
	// stderrBuf := bufio.NewReader(stderr)
	outFileName := fmt.Sprintf("%s.out.txt", path.Base(s.Path()))
	go outputWrite(stdout, stderr, path.Join(s.OutDir(ts), outFileName), s.ReadyStr, readyCh)
	if err := s.Cmd.Start(); err != nil {
		log.Panic(err)
	}
	s.waitCh = make(chan error)

	go func() {
		defer panicMain()
		// s.Running = true
		s.waitCh <- s.Cmd.Wait()
		// s.Running = false
	}()
}

func (s *Script) Run(ctx context.Context, args []string,
	ts string, readyCh chan bool) error {
	s.Start(ctx, args, ts, readyCh)
	return s.Wait()
}

func (s *Script) Wait() error {
	if s.Cmd == nil {
		return fmt.Errorf("Script.Cmd is nil")
	}
	err := <-s.waitCh
	log.Debugf("Finished %s with err: %v", s.Path(), err)
	return err
}

func (s *Script) Stop() error {
	if s.Cmd == nil {
		return fmt.Errorf("Script.Cmd is nil")
	}
	defer func() { s.Cmd = nil }()
	select {
	case res := <-s.waitCh:
		log.Debugf("DBG script PID: %v", s.Cmd.Process.Pid)
		return fmt.Errorf("Script exited prematurely with error %v",
			res)
	default:

	}

	pgid, err := syscall.Getpgid(s.Cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("Unable to get script process pgid")
	}
	if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil {
		return err
	}
	log.Debugf("SIGINT to %v", -pgid)
	select {
	case <-s.waitCh:
		return nil
	case <-time.After(30 * time.Second):
		syscall.Kill(-pgid, syscall.SIGKILL)
		return fmt.Errorf("Script took more than 30 seconds to terminate, killed")
	}
}

type Scripts struct {
	Setup       []*Script
	Start       []*Script
	Test        []*Script
	Stop        []*Script
	Hooks       []*Script
	PreludePath string
	RunDir      string
	OutDir      string
	CfgDir      string
	repoName    *string
}

func NewScriptsFromDir(dir, preludePath, runDir, outDir, cfgDir string, repoName *string,
	scriptsCfg map[string]ScriptCfg) (*Scripts, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	scripts := Scripts{
		PreludePath: preludePath,
		RunDir:      runDir,
		OutDir:      outDir,
		CfgDir:      cfgDir,
		repoName:    repoName,
	}
	for _, file := range files {
		filePath := path.Join(dir, file.Name())
		script := Script{
			PreludePath: &scripts.PreludePath,
			RunDir:      &scripts.RunDir,
			outDir:      &scripts.OutDir,
			CfgDir:      &scripts.CfgDir,
			repoName:    scripts.repoName,
			FileName:    file.Name(),
		}
		var scriptSlice *[]*Script
		if strings.HasPrefix(file.Name(), "setup") {
			scriptSlice = &scripts.Setup
		} else if strings.HasPrefix(file.Name(), "start") {
			scriptSlice = &scripts.Start
			if scriptsCfg == nil || scriptsCfg[file.Name()].ReadyStr == "" {
				return nil, fmt.Errorf("Start script %v hasn't specified a readystr",
					filePath)
			}
			script.ReadyStr = scriptsCfg[file.Name()].ReadyStr
		} else if strings.HasPrefix(file.Name(), "test") {
			scriptSlice = &scripts.Test
		} else if strings.HasPrefix(file.Name(), "stop") {
			scriptSlice = &scripts.Stop
		} else if strings.HasPrefix(file.Name(), "hook") {
			scriptSlice = &scripts.Hooks
		}
		if scriptSlice != nil {
			*scriptSlice = append(*scriptSlice, &script)
		}
	}
	return &scripts, nil
}

type Repo struct {
	GitRepo *git.Repository
	URL     string
	Branch  string
	Scripts Scripts
	// OutDir  string
	Dir string
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
	Dir          string
	CfgDir       string
	OutDir       string
	Timeouts     Timeouts
	Result       Result
	Repos        []*Repo
	Scripts      Scripts
	scriptsSetup []*Script
	scriptsStart []*Script
	scriptsTest  []*Script
	scriptsStop  []*Script
	scriptsHooks []*Script
}

func NewRepos(cfgDir, outDir, reposDir string, reposUrl []string,
	scriptsCfg map[string]map[string]ScriptCfg, reposCfg map[string]RepoCfg,
	timeouts Timeouts, skipClone bool) (*Repos, error) {
	repos := []*Repo{}
	for _, repoUrl := range reposUrl {
		name := repoUrl[strings.LastIndex(repoUrl, "/")+1:]
		repoDir := path.Join(reposDir, name)
		repo := Repo{URL: repoUrl, Branch: "master", Dir: repoDir}
		// repo.OutDir = path.Join(outDir, repo.Name())
		if reposCfg[repo.Name()].Branch != "" {
			repo.Branch = reposCfg[repo.Name()].Branch
		}
		var err error
		if skipClone {
			repo.GitRepo, err = git.PlainOpen(repo.Dir)
			if err != nil {
				return nil, err
			}
		} else {
			log.Infof("Cloning %s into %s", repo.URL, repo.Dir)
			repo.GitRepo, err = git.PlainClone(repo.Dir, false, &git.CloneOptions{
				URL: repo.URL,
				// SingleBranch:  true,
				ReferenceName: plumbing.NewBranchReferenceName(repo.Branch),
				Progress:      os.Stdout,
			})
			if err == git.ErrRepositoryAlreadyExists {
				log.Infof("Repository %s already exists", repo.URL)
				repo.GitRepo, err = git.PlainOpen(repo.Dir)
				if err != nil {
					return nil, err
				}
			} else if err != nil {
				return nil, err
			}
		}

		repoCfgDir := path.Join(cfgDir, repo.Name())
		repoName := repo.Name()
		scripts, err := NewScriptsFromDir(repoCfgDir,
			path.Join(cfgDir, "prelude"), repo.Dir, outDir,
			cfgDir, &repoName, scriptsCfg[repo.Name()])
		if err != nil {
			return nil, err
		}
		log.Infof("Loaded repository scripts at %v", repoCfgDir)
		repo.Scripts = *scripts
		repos = append(repos, &repo)
	}
	scripts, err := NewScriptsFromDir(cfgDir,
		path.Join(cfgDir, "prelude"), cfgDir, outDir, cfgDir, nil, scriptsCfg["global"])
	if err != nil {
		return nil, err
	}

	scriptsSetup := scripts.Setup
	for _, repo := range repos {
		scriptsSetup = append(scriptsSetup, repo.Scripts.Setup...)
	}
	scriptsStart := scripts.Start
	for _, repo := range repos {
		scriptsStart = append(scriptsStart, repo.Scripts.Start...)
	}
	scriptsTest := scripts.Test
	for _, repo := range repos {
		scriptsTest = append(scriptsTest, repo.Scripts.Test...)
	}
	scriptsStop := scripts.Stop
	for _, repo := range repos {
		scriptsStop = append(scriptsStop, repo.Scripts.Stop...)
	}
	scriptsHooks := scripts.Hooks
	for _, repo := range repos {
		scriptsHooks = append(scriptsHooks, repo.Scripts.Hooks...)
	}
	sort.Sort(ByFileName(scriptsSetup))
	sort.Sort(ByFileName(scriptsStart))
	sort.Sort(ByFileName(scriptsTest))
	sort.Sort(ByFileName(scriptsStop))
	sort.Sort(ByFileName(scriptsHooks))

	return &Repos{
		Repos:        repos,
		Dir:          reposDir,
		CfgDir:       cfgDir,
		OutDir:       outDir,
		Timeouts:     timeouts,
		Scripts:      *scripts,
		scriptsSetup: scriptsSetup,
		scriptsStart: scriptsStart,
		scriptsTest:  scriptsTest,
		scriptsStop:  scriptsStop,
		scriptsHooks: scriptsHooks,
	}, nil
}

func (rs *Repos) Update() (bool, error) {
	updated := false
	for _, repo := range rs.Repos {
		repoUrl := repo.URL
		oldHead, err := repo.HeadHash()
		if err != nil {
			return false, err
		}
		log.Infof("Updating %s", repoUrl)
		wt, err := repo.GitRepo.Worktree()
		if err != nil {
			return false, err
		}

		if err := repo.GitRepo.Fetch(&git.FetchOptions{
			Force: true,
			RefSpecs: []config.RefSpec{"refs/*:refs/*",
				"HEAD:refs/heads/HEAD"},
		}); err == git.NoErrAlreadyUpToDate {
			log.Infof("Repository %s already up-to-date", repoUrl)
		} else if err != nil {
			return false, err
		}

		if err := wt.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(repo.Branch),
			Force:  true,
		}); err != nil {
			return false, err
		}
		head, err := repo.HeadHash()
		if err != nil {
			return false, err
		}
		if oldHead != head {
			updated = true
		}
		log.Infof("Repository %s at commit %s", repoUrl, head)
	}
	return updated, nil
}

func outputWrite(stdout io.ReadCloser, stderr io.ReadCloser, filePath string,
	readyStr string, readyCh chan bool) {
	// log.Debugf("DBG Storing script output at %s", filePath)
	defer panicMain()
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Panic(err)
	}
	defer f.Close()
	lineCh := make(chan string)
	stdoutEndCh := make(chan error)
	stderrEndCh := make(chan error)
	readLine := func(rd io.Reader, endCh chan error) {
		defer panicMain()
		reader := bufio.NewReader(rd)
		for {
			line, err := reader.ReadString('\n')
			lineCh <- line
			if err != nil {
				break
			}
		}
		endCh <- err
	}
	go readLine(stdout, stdoutEndCh)
	go readLine(stderr, stderrEndCh)
	ready := false
	if readyStr == "" {
		ready = true
	}
	stdoutClosed, stderrClosed := false, false
	for {
		select {
		case line := <-lineCh:
			if !ready && strings.Contains(line, readyStr) {
				readyCh <- true
				ready = true
			}
			_, err := io.WriteString(f, line)
			if err != nil {
				log.Panicf("Can't write output to file %v: %v", filePath, err)
			}
			f.Sync()
		case err := <-stdoutEndCh:
			if err != nil && err != io.EOF {
				log.Errorf("Process stdout error: %v", err)
			}
			stdoutClosed = true
			if stderrClosed {
				return
			}
		case err := <-stderrEndCh:
			if err != nil && err != io.EOF {
				log.Errorf("Process stderr error: %v", err)
			}
			stderrClosed = true
			if stdoutClosed {
				return
			}
		}
	}
}

func (rs *Repos) ClearResults() {
	clearScripts := func(s *Scripts) {
		for _, script := range s.Setup {
			script.Result = ResultUnk
		}
		for _, script := range s.Start {
			script.Result = ResultUnk
		}
		for _, script := range s.Test {
			script.Result = ResultUnk
		}
		for _, script := range s.Stop {
			script.Result = ResultUnk
		}
		for _, script := range s.Hooks {
			script.Result = ResultUnk
		}
	}
	clearScripts(&rs.Scripts)
	for _, repo := range rs.Repos {
		clearScripts(&repo.Scripts)
	}
	rs.Result = ResultUnk
}

func (rs *Repos) ArchiveOld() {
	curLen := 16
	archiveDir := path.Join(rs.OutDir, "archive")
	if err := os.MkdirAll(archiveDir, 0777); err != nil {
		log.Panic(err)
	}
	resultDirs, err := getResultsDir(rs.OutDir)
	if err != nil {
		log.Panic(err)
	}
	if len(resultDirs) <= curLen {
		return
	}
	for _, file := range resultDirs[curLen:] {
		if err := os.Rename(path.Join(rs.OutDir, file), path.Join(archiveDir, file)); err != nil {
			log.Panic(err)
		}
	}
}

type WriteChan chan *[]byte

func NewWriteChan() WriteChan {
	return WriteChan(make(chan *[]byte))
}

func (w *WriteChan) Write(p []byte) (n int, err error) {
	*w <- &p
	return len(p), nil
}

func (w *WriteChan) Close() {
	// close(*w)
	*w <- nil
}

var dockerContainerID string

func (rs *Repos) DockerRun(cfg *Config) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.39"))
	if err != nil {
		log.Panic(err)
	}
	cli.NegotiateAPIVersion(ctx)

	if cfg.DockerCfg.Pull {
		log.Infof("Pulling docker image %v", cfg.DockerCfg.Image)
		rd, err := cli.ImagePull(ctx, cfg.DockerCfg.Image, types.ImagePullOptions{})
		if err != nil {
			log.Panic(err)
		}
		reader := bufio.NewReader(rd)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				log.Debugf("docker image pull: %s", line)
			}
			if err != nil {
				break
			}
		}
		rd.Close()
	}

	info := rs.NewInfo()
	containerName := fmt.Sprintf("citrus-%08d", info.Ts)

	// args := []string{"/bin/sh", "+ex", "-c", "/bin/ls -l /etc/ssl/certs"}
	args := []string{"/citrus/citrus", "-conf", "/citrus-cfg", "-no-web", "-no-update", "-one-shot"}
	if cfg.Debug {
		args = append(args, "-debug")
	}
	if cfg.Force {
		args = append(args, "-force")
	}
	mounts := []mount.Mount{
		{
			Type:     mount.TypeBind,
			Source:   cfg.CitrusDir,
			Target:   "/citrus",
			ReadOnly: true,
		},
		{
			Type:     mount.TypeBind,
			Source:   cfg.CfgDir,
			Target:   "/citrus-cfg",
			ReadOnly: true,
		},
		{
			Type:   mount.TypeBind,
			Source: fmt.Sprintf("%s/out", cfg.WorkDir),
			Target: fmt.Sprintf("%s/out", cfg.WorkDir),
		},
		{
			Type:   mount.TypeBind,
			Source: fmt.Sprintf("%s/git", cfg.WorkDir),
			Target: fmt.Sprintf("%s/git", cfg.WorkDir),
		},
	}
	for source, target := range cfg.DockerCfg.Binds {
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind,
			Source: source, Target: target, ReadOnly: true})
	}
	log.Infof("Creating docker container with name %s", containerName)
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Hostname:     "citrus",
			User:         "root",
			WorkingDir:   "/citrus",
			Image:        cfg.DockerCfg.Image,
			Cmd:          args,
			Tty:          true,
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			Mounts: mounts,
		},
		nil, containerName)
	if err != nil {
		log.Panic(err)
	}
	dockerContainerID = resp.ID
	defer func() {
		if err := cli.ContainerRemove(ctx, resp.ID,
			types.ContainerRemoveOptions{Force: true}); err != nil {
			log.Panic(err)
		}
		dockerContainerID = ""
	}()
	cont, err := cli.ContainerAttach(ctx, resp.ID, types.ContainerAttachOptions{
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		log.Panic(err)
	}

	log.Infof("Starting docker container with name %s, id %s", containerName, resp.ID)
	if err := cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		log.Panic(err)
	}

	reader := bufio.NewReader(cont.Reader)
	stdLog := log.StandardLogger()
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			io.WriteString(stdLog.Out, fmt.Sprintf("DOCKER %s", line))
		}
		if err != nil {
			break
		}
	}
	cont.Close()
	cont.CloseWrite()

	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			log.Panic(err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			log.Panicf("docker process terminated with exit code %v (%v)",
				status.StatusCode, status.Error)
		}
	}
}

func (rs *Repos) Run() {
	info := rs.NewInfo()
	outDir := path.Join(rs.OutDir, fmt.Sprintf("%08d", info.Ts))
	if err := os.MkdirAll(outDir, 0777); err != nil {
		log.Panic(err)
	}
	if err := rs.StoreInfo(&info, outDir); err != nil {
		log.Panic(err)
	}
	rs.ClearResults()

	f, err := os.Create(path.Join(outDir, ".running"))
	if err != nil {
		log.Panic(err)
	}
	f.Close()

	rs.run(&info, outDir)

	err = os.Remove(path.Join(outDir, ".running"))
	if err != nil {
		log.Panic(err)
	}

	log.Infof("Tests %08d (%s) result: %s", info.Ts, info.TsRFC3339, rs.Result)
	// rs.PrintResults()
	rs.ArchiveOld()
}

// NOTE: canceling the context of a script cmd only kills the parent but not
// the children, so cancelling is not a reliable way to stop everything that
// was started.  In the future, replace all cancel() by a robust Stop().
func (rs *Repos) run(info *Info, outDir string) {
	defer rs.StoreMapResult(outDir)
	ts := fmt.Sprintf("%08d", info.Ts)

	//// Hooks
	defer func() {
		for _, s := range rs.scriptsHooks {
			ctx, cancel := context.WithTimeout(context.Background(),
				rs.Timeouts.Hook*time.Second)
			defer cancel()
			if err := s.Run(ctx, []string{path.Join(outDir, "info.json"),
				path.Join(outDir, "result.json")}, ts, nil); err != nil {
				log.Errorf("Hook script %v error: %v", s.Path(), err)
			}
		}
	}()

	//// Setup
	log.Infof("--- Setup ---")
	for _, s := range rs.scriptsSetup {
		s.Result = ResultRun
		rs.StoreMapResult(outDir)
		ctx, cancel := context.WithTimeout(context.Background(),
			rs.Timeouts.Setup*time.Second)
		defer cancel()
		if err := s.Run(ctx, nil, ts, nil); err != nil {
			log.Errorf("Setup script %v error: %v", s.Path(), err)
			s.Result = ResultErr
			rs.Result = ResultErr
			return
		}
		s.Result = ResultPass
		rs.StoreMapResult(outDir)
	}

	//// Start
	log.Infof("--- Start ---")
	for _, s := range rs.scriptsStart {
		s.Result = ResultRun
		rs.StoreMapResult(outDir)
		readyCh := make(chan bool)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.Start(ctx, nil, ts, readyCh)
		select {
		case <-readyCh:
			s.Result = ResultReady
			log.Debugf("Start script %v is ready", s.Path())
		case <-time.After(rs.Timeouts.Ready * time.Second):
			cancel()
			log.Errorf("Start script %v timed out at ready", s.Path())
			s.Result = ResultErr
			rs.Result = ResultReadyErr
		}
		rs.StoreMapResult(outDir)
	}

	//// Test
	if rs.Result == ResultUnk {
		log.Infof("--- Test ---")
		for _, s := range rs.scriptsTest {
			s.Result = ResultRun
			rs.StoreMapResult(outDir)
			ctx, cancel := context.WithTimeout(context.Background(),
				rs.Timeouts.Test*time.Second)
			defer cancel()
			if err := s.Run(ctx, nil, ts, nil); err != nil {
				log.Errorf("Test script %v error: %v", s.Path(), err)
				s.Result = ResultErr
				rs.Result = ResultErr
			} else {
				s.Result = ResultPass
			}
			rs.StoreMapResult(outDir)
		}
	}

	//// Stop
	log.Infof("--- Stop ---")
	// Stop Start scripts
	for _, s := range rs.scriptsStart {
		s.Result = ResultRun
		rs.StoreMapResult(outDir)
		if err := s.Stop(); err != nil {
			log.Errorf("Error stopping Start script %v: %v", s.Path(), err)
			s.Result = ResultErr
			rs.Result = ResultErr
		} else {
			s.Result = ResultPass
		}
		rs.StoreMapResult(outDir)
	}

	for _, s := range rs.scriptsStop {
		s.Result = ResultRun
		rs.StoreMapResult(outDir)
		ctx, cancel := context.WithTimeout(context.Background(),
			rs.Timeouts.Stop*time.Second)
		defer cancel()
		if err := s.Run(ctx, nil, ts, nil); err != nil {
			log.Errorf("Stop script %v error: %v", s.Path(), err)
			s.Result = ResultErr
			rs.Result = ResultErr
		} else {
			s.Result = ResultPass
		}
		rs.StoreMapResult(outDir)
	}

	if rs.Result == ResultUnk {
		rs.Result = ResultPass
	}
	rs.StoreMapResult(outDir)

	return
}

// func (rs *Repos) PrintResults() {
// 	printOne := func(s *Script) {
// 		res := "UNK "
// 		switch s.Result {
// 		case ResultPass:
// 			res = "PASS"
// 		case ResultErr:
// 			res = "ERR "
// 		}
// 		fmt.Printf("%v - %v\n", res, strings.TrimPrefix(s.Path, rs.ConfDir))
// 	}
// 	printLoop := func(ss []*Script) {
// 		for _, s := range ss {
// 			printOne(s)
// 		}
// 	}
// 	printMaybe := func(s *Script) {
// 		if s != nil {
// 			printOne(s)
// 		}
// 	}
// 	fmt.Println("=== Setup ===")
// 	printMaybe(rs.Scripts.Setup)
// 	for _, r := range rs.Repos {
// 		fmt.Printf("= %v =\n", r.Name())
// 		printMaybe(r.Scripts.Setup)
// 	}
// 	fmt.Println("=== Start ===")
// 	for _, r := range rs.Repos {
// 		fmt.Printf("= %v =\n", r.Name())
// 		printLoop(r.Scripts.Start)
// 	}
// 	fmt.Println("=== Test ===")
// 	for _, r := range rs.Repos {
// 		fmt.Printf("= %v =\n", r.Name())
// 		printLoop(r.Scripts.Test)
// 	}
// 	fmt.Println("=== Stop ===")
// 	printMaybe(rs.Scripts.Stop)
// 	for _, r := range rs.Repos {
// 		fmt.Printf("= %v =\n", r.Name())
// 		printMaybe(r.Scripts.Stop)
// 	}
// }

type PhaseResults struct {
	Result map[string]Result
	Repos  map[string]map[string]Result
}

type MapResult struct {
	Result Result
	Setup  PhaseResults
	Start  PhaseResults
	Test   PhaseResults
	Stop   PhaseResults
}

func (rs *Repos) NewMapResult() MapResult {
	getPhaseResults := func(scripts []*Script) PhaseResults {
		result := make(map[string]Result)
		repos := make(map[string]map[string]Result)
		for _, s := range scripts {
			relPath := strings.Split(strings.TrimPrefix(
				strings.TrimPrefix(s.Path(), rs.CfgDir), "/"), "/")
			// log.Debugf("DBG NewMapResult s.Path: %v", s.Path())
			// log.Debugf("DBG NewMapResult rs.CfgDir: %v", rs.CfgDir)
			// log.Debugf("DBG NewMapResult relPath: %v", relPath)
			if len(relPath) == 1 {
				result[relPath[0]] = s.Result
			} else if len(relPath) == 2 {
				if _, ok := repos[relPath[0]]; !ok {
					repos[relPath[0]] = make(map[string]Result)
				}
				repos[relPath[0]][relPath[1]] = s.Result
			} else {
				panic("Unexpected relPath length")
			}
		}
		return PhaseResults{result, repos}
	}
	var result MapResult
	result.Setup = getPhaseResults(rs.scriptsSetup)
	result.Start = getPhaseResults(rs.scriptsStart)
	result.Test = getPhaseResults(rs.scriptsTest)
	result.Stop = getPhaseResults(rs.scriptsStop)
	result.Result = rs.Result
	return result
}

// StoreMapResult panics on error
func (rs *Repos) StoreMapResult(outDir string) {
	mapResult := rs.NewMapResult()
	resultJSON, err := json.Marshal(mapResult)
	if err != nil {
		log.Panic(err)
	}
	if err := ioutil.WriteFile(path.Join(outDir, "result.json"), resultJSON, 0666); err != nil {
		log.Panic(err)
	}
}

type InfoRepo struct {
	CommitHash string
	Branch     string
	URL        string
}

type Info struct {
	Repos     []InfoRepo
	Ts        int64
	TsRFC3339 string
}

// func (i *Info) TsRFC1123() string {
// 	return time.Unix(i.Ts, 0).Format(time.RFC1123)
// }

func (rs *Repos) NewInfo() Info {
	var info Info
	info.Repos = make([]InfoRepo, 0, len(rs.Repos))
	now := time.Now()
	info.Ts = now.Unix()
	info.TsRFC3339 = now.Format(time.RFC3339)
	for _, repo := range rs.Repos {
		hash, err := repo.HeadHash()
		if err != nil {
			log.Panic(err)
		}
		info.Repos = append(info.Repos, InfoRepo{
			CommitHash: hex.EncodeToString(hash[:]),
			Branch:     repo.Branch,
			URL:        repo.URL,
		})
	}
	return info
}

func (rs *Repos) StoreInfo(info *Info, outDir string) error {
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(outDir, "info.json"), infoJSON, 0666); err != nil {
		return err
	}
	return nil
}

func (rs *Repos) UpdateLoop(forceInit, once, skipUpdate bool, cfg *Config) {
	defer panicMain()
	var err error
	var updateInit bool
	if !skipUpdate {
		updateInit, err = rs.Update()
		if err != nil {
			log.Panic(err)
		}
	}
	updated := forceInit || updateInit || once
	for {
		if updated {
			if cfg.Docker {
				rs.DockerRun(cfg)
			} else {
				rs.Run()
			}
			// rs.PrintResults()
			if once {
				return
			}
		}
		time.Sleep(rs.Timeouts.Loop * time.Second)
		updated, err = rs.Update()
		if err != nil {
			log.Panic(err)
		}
	}
}

func (rs *Repos) Cleanup() {
	log.Info("Stopping start Scripts...")
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Start {
			script.Stop()
		}
	}
}

func printUsage() {
	fmt.Printf("Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func getResultsDir(outDir string) ([]string, error) {
	files, err := ioutil.ReadDir(outDir)
	if err != nil {
		return nil, err
	}
	resultDirs := make([]string, 0, len(files))
	for _, file := range files {
		if file.Name() == "archive" {
			continue
		}
		if !file.IsDir() {
			continue
		}
		resultDirs = append(resultDirs, file.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(resultDirs)))
	return resultDirs, nil
}

type InfoRes struct {
	Info    Info
	Res     *MapResult
	Running bool
}

func getInfoRes(dir string) (*InfoRes, error) {
	var infoRes InfoRes
	infoFile, err := os.Open(path.Join(dir, "info.json"))
	if err != nil {
		return nil, nil
	}
	defer infoFile.Close()
	var info Info
	if err := json.NewDecoder(infoFile).Decode(&info); err != nil {
		return nil, err
	}
	infoRes.Info = info
	resultFile, err := os.Open(path.Join(dir, "result.json"))
	if err == nil {
		defer resultFile.Close()
		var result MapResult
		if err := json.NewDecoder(resultFile).Decode(&result); err != nil {
			return nil, err
		}
		infoRes.Res = &result
	}
	if _, err := os.Stat(path.Join(dir, ".running")); os.IsNotExist(err) || err != nil {
		infoRes.Running = false
	} else {
		infoRes.Running = true
	}
	return &infoRes, nil
}

func cleanRunningFiles(outDir string) error {
	resultDirs, err := getResultsDir(outDir)
	if err != nil {
		return err
	}
	for _, resultDir := range resultDirs {
		dir := path.Join(outDir, resultDir)
		os.Remove(path.Join(dir, ".running"))
	}
	return nil
}

func main() {
	panicCh = make(chan interface{})

	cfgDir := flag.String("conf", "", "config directory")
	debug := flag.Bool("debug", false, "enable debug output")
	quiet := flag.Bool("quiet", false, "output warnings and errors only")
	webOnly := flag.Bool("web-only", false, "run web backend only")
	noWeb := flag.Bool("no-web", false, "don't run the web backend")
	force := flag.Bool("force", false, "force an initial run even if repositories were not updated")
	oneShot := flag.Bool("one-shot", false, "run tests only once")
	docker := flag.Bool("docker", false, "run tests in a docker container")
	noUpdate := flag.Bool("no-update", false, "don't update the repositories during first loop")
	flag.Parse()
	if *cfgDir == "" {
		printUsage()
		return
	}
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
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
	if !path.IsAbs(*cfgDir) {
		*cfgDir = path.Join(wd, *cfgDir)
	}
	viper.SetConfigType("toml")
	viper.SetConfigName("config")
	viper.AddConfigPath(*cfgDir)

	if err := viper.ReadInConfig(); err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatal(err)
	}
	cfg.CfgDir = strings.TrimSuffix(*cfgDir, "/")
	cfg.CitrusDir = wd
	cfg.Debug = *debug
	cfg.Force = *force
	cfg.Docker = *docker

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

	// Clean unfinished results
	if err := cleanRunningFiles(outDir); err != nil {
		log.Fatal(err)
	}

	var repos *Repos
	if !*webOnly {
		repos, err = NewRepos(cfg.CfgDir, outDir, reposDir, cfg.ReposUrl,
			cfg.ScriptsCfg,
			cfg.ReposCfg,
			Timeouts{
				Loop:  time.Duration(cfg.Timeouts.Loop),
				Setup: time.Duration(cfg.Timeouts.Setup),
				Ready: time.Duration(cfg.Timeouts.Ready),
				Test:  time.Duration(cfg.Timeouts.Test),
				Stop:  time.Duration(cfg.Timeouts.Stop),
				Hook:  time.Duration(cfg.Timeouts.Hook),
			},
			*noUpdate,
		)
		if err != nil {
			log.Fatal(err)
		}
		repos.ArchiveOld()
	}

	stop := make(chan bool)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	if !*webOnly {
		if *oneShot && *noWeb {
			go func() {
				repos.UpdateLoop(*force, *oneShot, *noUpdate, &cfg)
				stop <- true
			}()
		} else {
			go repos.UpdateLoop(*force, *oneShot, *noUpdate, &cfg)
		}
	}

	if !*noWeb {
		go serveWeb(outDir, cfg.Listen)
	}

	var panicVal interface{}
	select {
	case <-sigCh:
	case panicVal = <-panicCh:
	case <-stop:
		log.Info("UpdateLoop finished")
	}
	if repos != nil && !*docker {
		repos.Cleanup()
	}
	if *docker {
		err := func() error {
			if dockerContainerID == "" {
				return nil
			}
			log.Info("Cleaning docker environment...")
			ctx := context.Background()
			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.39"))
			if err != nil {
				return err
			}
			cli.NegotiateAPIVersion(ctx)
			log.Info("Stopping docker container...")
			timeout := time.Duration(10 * time.Second)
			if err := cli.ContainerStop(ctx, dockerContainerID,
				&timeout); err != nil {
				return err
			}
			if err := cli.ContainerRemove(ctx, dockerContainerID,
				types.ContainerRemoveOptions{Force: true}); err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			log.Error(err)
		}
	}
	if panicVal != nil {
		log.Fatal(panicVal)
	}
	log.Info("Exiting...")
}
