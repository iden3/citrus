package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/gin-gonic/gin"

	// "net/http"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

type Result string

const (
	ResultUnk      Result = "UNK"
	ResultPass     Result = "PASS"
	ResultErr      Result = "ERR"
	ResultReady    Result = "READY"
	ResultReadyErr Result = "READYERR"
	ResultRun      Result = "RUNNING"
)

type ScriptCfg struct {
	ReadyStr string
}

type RepoCfg struct {
	Branch string
}

type Config struct {
	WorkDir  string
	ConfDir  string `mapstructure:"-"`
	Listen   string
	ReposUrl []string `mapstructure:"repos"`
	Timeouts struct {
		Loop  int64
		Setup int64
		Ready int64
		Test  int64
		Stop  int64
	}
	ScriptsCfg map[string]map[string]ScriptCfg `mapstructure:"script"`
	ReposCfg   map[string]RepoCfg              `mapstructure:"repo"`
}

type Timeouts struct {
	Loop  time.Duration
	Setup time.Duration
	Ready time.Duration
	Test  time.Duration
	Stop  time.Duration
}

type Script struct {
	Path     string
	ReadyStr string
	Cmd      *exec.Cmd
	Result   Result
	waitCh   chan error
	// Running  bool
}

func (s *Script) Run(ctx context.Context, preludePath, runPath, outDir string,
	readyCh chan bool) {
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
	s.waitCh = make(chan error)
	go func() {
		// s.Running = true
		s.waitCh <- s.Cmd.Wait()
		// s.Running = false
	}()
	outFileName := fmt.Sprintf("%s.out.txt", path.Base(s.Path))
	outputWrite(stdout, stderr, path.Join(outDir, outFileName), s.ReadyStr, readyCh)
}

func (s *Script) Wait() error {
	err := <-s.waitCh
	log.Debugf("Finished %s with err: %v", s.Path, err)
	return err
}

func (s *Script) Stop() error {
	defer func() { s.Cmd = nil }()
	select {
	case res := <-s.waitCh:
		return fmt.Errorf("Script exited prematurely with error %v",
			res)
	case <-time.After(500 * time.Millisecond):

	}

	pgid, err := syscall.Getpgid(s.Cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("Unable to get script process pgid")
	}
	if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil {
		return err
	}
	select {
	case <-s.waitCh:
		return nil
	case <-time.After(30 * time.Second):
		syscall.Kill(-pgid, syscall.SIGKILL)
		return fmt.Errorf("Script took more than 30 seconds to terminate, killed")
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
	Branch  string
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
	Result   Result
}

func NewRepos(confDir, outDir, reposDir string, reposUrl []string,
	scriptsCfg map[string]map[string]ScriptCfg, reposCfg map[string]RepoCfg,
	timeouts Timeouts) (*Repos, error) {
	repos := []*Repo{}
	for _, repoUrl := range reposUrl {
		name := repoUrl[strings.LastIndex(repoUrl, "/")+1:]
		repoDir := path.Join(reposDir, name)
		repo := Repo{URL: repoUrl, Branch: "master", Dir: repoDir}
		if reposCfg[repo.Name()].Branch != "" {
			repo.Branch = reposCfg[repo.Name()].Branch
		}
		log.Infof("Cloning %s into %s", repo.URL, repo.Dir)
		gitRepo, err := git.PlainClone(repo.Dir, false, &git.CloneOptions{
			URL: repo.URL,
			// SingleBranch:  true,
			ReferenceName: plumbing.NewBranchReferenceName(repo.Branch),
			Progress:      os.Stdout,
		})
		if err == git.ErrRepositoryAlreadyExists {
			log.Infof("Repository %s already exists", repo.URL)
			gitRepo, err = git.PlainOpen(repo.Dir)
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		repo.GitRepo = gitRepo

		repoConfDir := path.Join(confDir, repo.Name())
		files, err := ioutil.ReadDir(repoConfDir)
		if err != nil {
			log.Warn("DBG conf dir not found, skipping...")
			continue
			// return nil, err
		}
		var scripts Scripts
		cfg := scriptsCfg[repo.Name()]
		for _, file := range files {
			filePath := path.Join(repoConfDir, file.Name())
			if strings.HasPrefix(file.Name(), "setup") {
				scripts.Setup = &Script{Path: filePath}
			} else if strings.HasPrefix(file.Name(), "start") {
				if cfg == nil || cfg[file.Name()].ReadyStr == "" {
					log.Fatalf("Start %v -> %v hasn't specified a readystr", repo.Name(), filePath)
				}
				script := Script{Path: filePath}
				script.ReadyStr = cfg[file.Name()].ReadyStr
				scripts.Start = append(scripts.Start, &script)
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
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0600)
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
				break
			}
		}
		endCh <- err
	}
	go readLine(stdout)
	go readLine(stderr)
	ready := false
	if readyStr == "" {
		ready = true
	}
	for {
		select {
		case line := <-lineCh:
			if !ready && strings.Contains(line, readyStr) {
				readyCh <- true
				ready = true
			}
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

func (rs *Repos) ClearResults() {
	clearScripts := func(s *Scripts) {
		if s.Setup != nil {
			s.Setup.Result = ResultUnk
		}
		for _, script := range s.Start {
			script.Result = ResultUnk
		}
		for _, script := range s.Test {
			script.Result = ResultUnk
		}
		if s.Stop != nil {
			s.Stop.Result = ResultUnk
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
	if err := os.MkdirAll(archiveDir, 0700); err != nil {
		log.Fatal(err)
	}
	resultDirs, err := getResultsDir(rs.OutDir)
	if err != nil {
		log.Fatal(err)
	}
	if len(resultDirs) <= curLen {
		return
	}
	for _, file := range resultDirs[curLen:] {
		if err := os.Rename(path.Join(rs.OutDir, file), path.Join(archiveDir, file)); err != nil {
			log.Fatal(err)
		}
	}
}

func (rs *Repos) Run() {
	info := rs.NewInfo()
	outDir := path.Join(rs.OutDir, fmt.Sprintf("%08d", info.Ts))
	if err := os.MkdirAll(outDir, 0700); err != nil {
		log.Fatal(err)
	}
	if err := rs.StoreInfo(&info, outDir); err != nil {
		log.Fatal(err)
	}
	rs.ClearResults()

	rs.run(&info, outDir)

	log.Infof("Tests %08d (%s) result: %s", info.Ts, info.TsRFC3339, rs.Result)

	rs.PrintResults()
	if err := rs.StoreMapResult(outDir); err != nil {
		log.Fatal(err)
	}

	rs.ArchiveOld()
}

func (rs *Repos) run(info *Info, outDir string) {

	//// Setup
	log.Infof("--- Setup ---")
	ctxSetup, cancelSetup := context.WithTimeout(context.Background(),
		rs.Timeouts.Setup*time.Second)
	defer cancelSetup()
	rs.Scripts.Setup.Result = ResultRun
	if err := rs.StoreMapResult(outDir); err != nil {
		log.Fatal(err)
	}
	rs.Scripts.Setup.Run(ctxSetup, rs.Scripts.PreludePath, "", outDir, nil)
	if err := rs.Scripts.Setup.Wait(); err != nil {
		log.Errorf("Scripts.Setup error: %v", err)
		rs.Scripts.Setup.Result = ResultErr
		rs.Result = ResultErr
		return
	}
	rs.Scripts.Setup.Result = ResultPass

	for _, repo := range rs.Repos {
		script := repo.Scripts.Setup
		if script == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(),
			rs.Timeouts.Setup*time.Second)
		defer cancel()
		script.Result = ResultRun
		if err := rs.StoreMapResult(outDir); err != nil {
			log.Fatal(err)
		}
		script.Run(ctx, rs.Scripts.PreludePath, repo.Dir,
			path.Join(outDir, repo.Name()), nil)
		if err := script.Wait(); err != nil {
			log.Errorf("Setup %v -> %v finished with error: %v",
				repo.Name(), script.Path, err)
			script.Result = ResultErr
			rs.Result = ResultErr
			return
		}
		script.Result = ResultPass
		if err := rs.StoreMapResult(outDir); err != nil {
			log.Fatal(err)
		}
	}

	//// Start
	log.Infof("--- Start ---")
	ctxStart, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Start {
			ctx, cancel := context.WithCancel(ctxStart)
			defer cancel()
			s := script
			repoName := repo.Name()
			readyCh := make(chan bool)
			script.Result = ResultRun
			if err := rs.StoreMapResult(outDir); err != nil {
				log.Fatal(err)
			}
			go func() {
				s.Run(ctx, rs.Scripts.PreludePath, repo.Dir,
					path.Join(outDir, repoName), readyCh)
			}()
			select {
			case <-readyCh:
				script.Result = ResultReady
				log.Debugf("Start %v -> %v is ready", repo.Name(), script.Path)
			case <-time.After(rs.Timeouts.Ready * time.Second):
				cancel()
				log.Errorf("Start %v -> %v timed out at ready",
					repo.Name(), script.Path)
				script.Result = ResultErr
				rs.Result = ResultReadyErr
			}
			if err := rs.StoreMapResult(outDir); err != nil {
				log.Fatal(err)
			}
		}
	}

	//// Test
	if rs.Result == ResultUnk {
		log.Infof("--- Test ---")
		for _, repo := range rs.Repos {
			for _, script := range repo.Scripts.Test {
				ctx, cancel := context.WithTimeout(context.Background(),
					rs.Timeouts.Test*time.Second)
				defer cancel()
				script.Result = ResultRun
				if err := rs.StoreMapResult(outDir); err != nil {
					log.Fatal(err)
				}
				script.Run(ctx, rs.Scripts.PreludePath, repo.Dir,
					path.Join(outDir, repo.Name()), nil)
				if err := script.Wait(); err != nil {
					log.Errorf("Test %v -> %v finished with error: %v",
						repo.Name(), script.Path, err)
					script.Result = ResultErr
					rs.Result = ResultErr
				} else {
					script.Result = ResultPass
				}
				if err := rs.StoreMapResult(outDir); err != nil {
					log.Fatal(err)
				}
			}
		}
	}

	//// Stop
	log.Infof("--- Stop ---")
	ctxStop, cancelStop := context.WithTimeout(context.Background(),
		rs.Timeouts.Stop*time.Second)
	defer cancelStop()
	rs.Scripts.Stop.Result = ResultRun
	if err := rs.StoreMapResult(outDir); err != nil {
		log.Fatal(err)
	}
	rs.Scripts.Stop.Run(ctxStop, rs.Scripts.PreludePath, "", outDir, nil)
	if err := rs.Scripts.Stop.Wait(); err != nil {
		log.Errorf("Scripts.Stop error: %v", err)
		rs.Scripts.Stop.Result = ResultErr
		rs.Result = ResultErr
		return
	}
	rs.Scripts.Stop.Result = ResultPass

	// Stop Start scripts
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Start {
			if err := script.Stop(); err != nil {
				log.Errorf("Error stopping Start %v -> %v: %v", repo.Name(),
					script.Path, err)
				script.Result = ResultErr
				rs.Result = ResultErr
			} else {
				script.Result = ResultPass
			}
		}
	}

	for _, repo := range rs.Repos {
		script := repo.Scripts.Stop
		if script == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(),
			rs.Timeouts.Stop*time.Second)
		defer cancel()
		script.Result = ResultRun
		if err := rs.StoreMapResult(outDir); err != nil {
			log.Fatal(err)
		}
		script.Run(ctx, rs.Scripts.PreludePath, repo.Dir,
			path.Join(outDir, repo.Name()), nil)
		if err := script.Wait(); err != nil {
			log.Errorf("Stop %v -> %v finished with error: %v",
				repo.Name(), script.Path, err)
			script.Result = ResultErr
			rs.Result = ResultErr
		} else {
			script.Result = ResultPass
		}
	}
	if rs.Result == ResultUnk {
		rs.Result = ResultPass
	}

	return
}

func (rs *Repos) PrintResults() {
	printOne := func(s *Script) {
		res := "UNK "
		switch s.Result {
		case ResultPass:
			res = "PASS"
		case ResultErr:
			res = "ERR "
		}
		fmt.Printf("%v - %v\n", res, strings.TrimPrefix(s.Path, rs.ConfDir))
	}
	printLoop := func(ss []*Script) {
		for _, s := range ss {
			printOne(s)
		}
	}
	printMaybe := func(s *Script) {
		if s != nil {
			printOne(s)
		}
	}
	fmt.Println("=== Setup ===")
	printMaybe(rs.Scripts.Setup)
	for _, r := range rs.Repos {
		fmt.Printf("= %v =\n", r.Name())
		printMaybe(r.Scripts.Setup)
	}
	fmt.Println("=== Start ===")
	for _, r := range rs.Repos {
		fmt.Printf("= %v =\n", r.Name())
		printLoop(r.Scripts.Start)
	}
	fmt.Println("=== Test ===")
	for _, r := range rs.Repos {
		fmt.Printf("= %v =\n", r.Name())
		printLoop(r.Scripts.Test)
	}
	fmt.Println("=== Stop ===")
	printMaybe(rs.Scripts.Stop)
	for _, r := range rs.Repos {
		fmt.Printf("= %v =\n", r.Name())
		printMaybe(r.Scripts.Stop)
	}
}

type MapResult struct {
	Result Result
	Setup  struct {
		Result *Result
		Repos  map[string]Result
	}
	Start struct {
		// Result *Result
		Repos map[string]map[string]Result
	}
	Test struct {
		// Result *Result
		Repos map[string]map[string]Result
	}
	Stop struct {
		Result *Result
		Repos  map[string]Result
	}
}

func (rs *Repos) NewMapResult() MapResult {
	getSingle := func(s *Script) *Result {
		if s != nil {
			res := s.Result
			return &res
		}
		return nil
	}
	getRepoSingle := func(repos []*Repo, phase string) map[string]Result {
		m := make(map[string]Result)
		for _, repo := range rs.Repos {
			var script *Script
			switch phase {
			case "setup":

				script = repo.Scripts.Setup
			case "stop":
				script = repo.Scripts.Stop
			}
			if script != nil {
				m[repo.Name()] = script.Result
			}
		}
		return m
	}
	getRepoMulti := func(repos []*Repo, phase string) map[string]map[string]Result {
		m := make(map[string]map[string]Result)
		for _, repo := range rs.Repos {
			var scripts []*Script
			switch phase {
			case "start":
				scripts = repo.Scripts.Start
			case "test":
				scripts = repo.Scripts.Test
			}
			ms := make(map[string]Result)
			for _, s := range scripts {
				scriptName := strings.TrimPrefix(strings.TrimPrefix(s.Path, rs.ConfDir), "/")
				ms[scriptName] = s.Result
			}
			if len(ms) != 0 {
				m[repo.Name()] = ms
			}
		}
		return m
	}
	var result MapResult
	result.Setup.Result = getSingle(rs.Scripts.Setup)
	result.Setup.Repos = getRepoSingle(rs.Repos, "setup")
	result.Start.Repos = getRepoMulti(rs.Repos, "start")
	result.Test.Repos = getRepoMulti(rs.Repos, "test")
	result.Stop.Result = getSingle(rs.Scripts.Stop)
	result.Stop.Repos = getRepoSingle(rs.Repos, "stop")
	result.Result = rs.Result

	return result
}

func (rs *Repos) StoreMapResult(outDir string) error {
	mapResult := rs.NewMapResult()
	resultJSON, err := json.Marshal(mapResult)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(outDir, "result.json"), resultJSON, 0600); err != nil {
		return err
	}
	return nil
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
			log.Fatal(err)
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
	if err := ioutil.WriteFile(path.Join(outDir, "info.json"), infoJSON, 0600); err != nil {
		return err
	}
	return nil
}

func (rs *Repos) UpdateLoop() {
	var err error
	_, err = rs.Update()
	if err != nil {
		log.Fatal(err)
	}
	updated := true
	for {
		if updated {
			rs.Run()
			// rs.PrintResults()
		}
		time.Sleep(rs.Timeouts.Loop * time.Second)
		updated, err = rs.Update()
		if err != nil {
			log.Fatal(err)
		}
	}
}

func ginFail(c *gin.Context, msg string, err error) {
	if err != nil {
		log.WithError(err).Error(msg)
	} else {
		log.Error(msg)
	}
	c.JSON(400, gin.H{
		"error": fmt.Sprintf("%s: %v", msg, err),
	})
	return
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
	Info Info
	Res  *MapResult
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
	return &infoRes, nil
}

func main() {
	confDir := flag.String("conf", "", "config directory")
	debug := flag.Bool("debug", false, "enable debug output")
	quiet := flag.Bool("quiet", false, "output warnings and errors only")
	test := flag.Bool("test", false, "run web backend only")
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
	// fmt.Printf("%#v\n", cfg)

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

	if !*test {
		repos, err := NewRepos(cfg.ConfDir, outDir, reposDir, cfg.ReposUrl,
			cfg.ScriptsCfg,
			cfg.ReposCfg,
			Timeouts{
				Loop:  time.Duration(cfg.Timeouts.Loop),
				Setup: time.Duration(cfg.Timeouts.Setup),
				Ready: time.Duration(cfg.Timeouts.Ready),
				Test:  time.Duration(cfg.Timeouts.Test),
				Stop:  time.Duration(cfg.Timeouts.Stop),
			},
		)
		if err != nil {
			log.Fatal(err)
		}

		go repos.UpdateLoop()
	}

	// http.Handle("/", http.FileServer(http.Dir(cfg.WorkDir)))

	// log.Infof("Serving %s on http://%s", cfg.WorkDir, cfg.Listen)
	// log.Fatal(http.ListenAndServe(cfg.Listen, nil))

	router := gin.Default()
	router.SetFuncMap(template.FuncMap{
		"RFC1123": func(t int64) string {
			return time.Unix(t, 0).Format(time.RFC1123)
		},
		"RepoName": func(url string) string {
			return url[strings.LastIndex(url, "/")+1:]
		},
		"Res2Icon": func(res Result) string {
			switch res {
			case ResultErr:
				return "☒"
			case ResultPass:
				return "☑"
			case ResultReadyErr:
				return "☒"
			case ResultReady:
				return "✌"
			case ResultRun:
				return "☛"
			default:
				return "♨"
			}
		},
		"ResultUnk": func() Result {
			return ResultUnk
		},
	})

	router.LoadHTMLGlob(path.Join(wd, "assets", "templates", "*"))
	router.Static("static", path.Join(wd, "assets", "static"))
	router.Static("out", outDir)

	router.GET("/", func(c *gin.Context) {
		resultDirs, err := getResultsDir(outDir)
		if err != nil {
			ginFail(c, "", err)
			return
		}
		tests := make([]InfoRes, 0, len(resultDirs))
		for _, resultDir := range resultDirs {
			dir := path.Join(outDir, resultDir)
			infoRes, err := getInfoRes(dir)
			if err != nil {
				ginFail(c, "", err)
				return
			}
			if infoRes != nil {
				tests = append(tests, *infoRes)
			}
		}
		c.HTML(http.StatusOK, "index.html", gin.H{
			"tests": tests,
		})
	})

	handleResult := func(c *gin.Context, ts string) error {
		infoRes, err := getInfoRes(path.Join(outDir, ts))
		if err != nil {
			ginFail(c, "", err)
			return err
		}
		if infoRes == nil {
			infoRes, err = getInfoRes(path.Join(outDir, "archive", ts))
			if err != nil {
				ginFail(c, "", err)
				return err
			}
		}
		if infoRes == nil {
			err := fmt.Errorf("No info.json found")
			ginFail(c, "", err)
			return err
		}
		fmt.Printf("DBG %#v", infoRes.Res.Setup.Repos)
		c.HTML(http.StatusOK, "result.html", gin.H{
			"infoRes": *infoRes,
		})
		return nil
	}

	router.GET("/result/:ts", func(c *gin.Context) {
		ts := c.Param("ts")
		if strings.Contains(ts, "/") || strings.Contains(ts, "..") {
			ginFail(c, "", fmt.Errorf("ts contains \"/\" or \"..\""))
			return
		}
		handleResult(c, ts)
	})

	router.GET("/last", func(c *gin.Context) {
		resultDirs, err := getResultsDir(outDir)
		if err != nil {
			ginFail(c, "", err)
			return
		}
		ts := resultDirs[0]
		handleResult(c, ts)
	})

	log.Infof("Listening on http://%s", cfg.Listen)
	router.Run(cfg.Listen)
}
