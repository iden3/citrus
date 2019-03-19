package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

const workDir = "/tmp/iden3-CIT"
const confDir = "/home/dev/git/iden3/continuous-integration-testing/tmp"
const port = "8010"
const loopTimeout = 60
const testTimeout = 120
const setupTimeout = 120

type Timeouts struct {
	Loop  time.Duration
	Test  time.Duration
	Setup time.Duration
}

type Scripts struct {
	Setup   string
	Start   []string
	Test    []string
	Stop    string
	Prelude string
}

type Repo struct {
	GitRepo *git.Repository
	URL     string
	Scripts Scripts
	OutDir  string
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
		log.Printf("Cloning %s into %s", repoUrl, repoDir)
		gitRepo, err := git.PlainClone(repoDir, false, &git.CloneOptions{
			SingleBranch: true,
			URL:          repoUrl,
			Progress:     os.Stdout,
		})
		if err == git.ErrRepositoryAlreadyExists {
			log.Printf("Repository %s already exists", repoUrl)
			gitRepo, err = git.PlainOpen(repoDir)
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		repo := Repo{GitRepo: gitRepo, URL: repoUrl}

		repoConfDir := path.Join(confDir, repo.Name())
		files, err := ioutil.ReadDir(repoConfDir)
		if err != nil {
			log.Printf("DBG conf dir not found, skipping...")
			continue
			// return nil, err
		}
		var scripts Scripts
		for _, file := range files {
			filePath := path.Join(repoConfDir, file.Name())
			if strings.HasPrefix(file.Name(), "setup") {
				scripts.Setup = filePath
			} else if strings.HasPrefix(file.Name(), "start") {
				scripts.Start = append(scripts.Start, filePath)
			} else if strings.HasPrefix(file.Name(), "test") {
				scripts.Test = append(scripts.Test, filePath)
			} else if strings.HasPrefix(file.Name(), "stop") {
				scripts.Stop = filePath
			}
		}
		scripts.Prelude = path.Join(confDir, "prelude")
		log.Printf("Loaded repository scripts: %#v", scripts)
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
			Setup:   path.Join(confDir, "setup"),
			Stop:    path.Join(confDir, "stop"),
			Prelude: path.Join(confDir, "prelude"),
		},
	}, nil
}

func (rs *Repos) Update() (bool, error) {
	updated := false
	for _, repo := range rs.Repos {
		repoUrl := repo.URL
		log.Printf("Updating %s", repoUrl)
		wt, err := repo.GitRepo.Worktree()
		if err != nil {
			return false, err
		}
		if err := wt.Pull(&git.PullOptions{
			SingleBranch: true,
			Progress:     os.Stdout,
			Force:        true,
		}); err == git.NoErrAlreadyUpToDate {
			log.Printf("Repository %s already up-to-date", repoUrl)
		} else if err != nil {
			return false, err
		} else {
			updated = true
		}
		head, err := repo.HeadHash()
		if err != nil {
			return false, err
		}
		log.Printf("Repository %s at commit %s", repoUrl, head)
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
		if ok && pathErr.Op != "read" {
			log.Fatalf("%#v", err)
		}
	}
}

func (rs *Repos) RunScript(ctx context.Context, scriptPath string, outDir string) error {
	if err := os.MkdirAll(outDir, 0700); err != nil {
		log.Fatal(err)
	}
	log.Printf("Running %s", scriptPath)
	cmd := exec.CommandContext(ctx, rs.Scripts.Prelude, scriptPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	stdoutFileName := fmt.Sprintf("%s.stdout.txt", path.Base(scriptPath))
	go writeOutFile(stdout, path.Join(outDir, stdoutFileName))
	stderrFileName := fmt.Sprintf("%s.stderr.txt", path.Base(scriptPath))
	go writeOutFile(stderr, path.Join(outDir, stderrFileName))
	err = cmd.Wait()
	log.Printf("Finished %s", scriptPath)
	return err
}

func (rs *Repos) Run() {
	now := time.Now()
	ts := fmt.Sprintf("%08d_%s", now.Unix(), now.Format(time.RFC3339))
	outDir := path.Join(rs.OutDir, ts)

	// Setup
	ctx, cancel := context.WithTimeout(context.Background(),
		rs.Timeouts.Setup*time.Second)
	defer cancel()
	err := rs.RunScript(ctx, rs.Scripts.Setup, outDir)
	log.Printf("Command finished with error: %v", err)

	// Start
	cancelsStartAll := make(map[string][]context.CancelFunc)
	errStartAll := make(map[string][]error)
	for _, repo := range rs.Repos {
		cancels := make([]context.CancelFunc, len(repo.Scripts.Start))
		cancelsStartAll[repo.Name()] = cancels
		errs := make([]error, len(repo.Scripts.Start))
		errStartAll[repo.Name()] = errs
		for i, script := range repo.Scripts.Start {
			ctx, cancel := context.WithCancel(context.Background())
			cancels[i] = cancel
			n, scriptPath, repoName := i, script, repo.Name()
			go func() {
				errs[n] = rs.RunScript(ctx, scriptPath,
					path.Join(outDir, repoName))
			}()
		}
	}

	// Test
	for _, repo := range rs.Repos {
		for _, script := range repo.Scripts.Test {
			ctx, cancel := context.WithTimeout(context.Background(),
				rs.Timeouts.Test*time.Second)
			defer cancel()
			err := rs.RunScript(ctx, script, path.Join(outDir, repo.Name()))
			log.Printf("Command finished with error: %v", err)
		}
	}

	// Stop

	for name, cancels := range cancelsStartAll {
		errs := errStartAll[name]
		for i, cancel := range cancels {
			err := errs[i]
			if err != nil {
				log.Printf("Start %v returned error: %v", name, err)
			} else {
				log.Printf("Start %v ran successfully", name)
			}
			cancel()
		}
	}
}

func (rs *Repos) UpdateLoop() {
	rs.Run()
	for {
		updated, err := rs.Update()
		if err != nil {
			log.Fatal(err)
		}
		if updated {
			rs.Run()
		}
		time.Sleep(rs.Timeouts.Loop * time.Second)
	}
}

func main() {
	reposUrl := []string{
		"https://github.com/iden3/go-iden3.git",
		"https://github.com/iden3/iden3js.git",
		"https://github.com/iden3/notifications-server.git",
		"https://github.com/iden3/tx-forwarder.git",
	}
	if err := os.MkdirAll(workDir, 0700); err != nil {
		log.Fatal(err)
	}
	reposDir := path.Join(workDir, "git")
	if err := os.MkdirAll(reposDir, 0700); err != nil {
		log.Fatal(err)
	}
	outDir := path.Join(workDir, "out")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		log.Fatal(err)
	}

	repos, err := NewRepos(confDir, outDir, reposDir, reposUrl,
		Timeouts{
			Loop:  time.Duration(loopTimeout),
			Setup: time.Duration(setupTimeout),
			Test:  time.Duration(testTimeout),
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	go repos.UpdateLoop()

	http.Handle("/", http.FileServer(http.Dir(workDir)))

	log.Printf("Serving %s on http://127.0.0.1:%s", workDir, port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
