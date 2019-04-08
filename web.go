package main

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

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

type PipelineDecorator struct {
	// The actual pipeline
	Data interface{}
	// Some helper data passed as "second pipeline"
	Deco interface{}
}

func serveWeb(outDir, listenAddr string) {
	defer panicMain()
	wd, err := os.Getwd()
	if err != nil {
		log.Panic(err)
	}
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
			case ResultUnfinished:
				return "☠"
			default:
				return "♨"
			}
		},
		"ResultUnfinished": func() Result {
			return ResultUnfinished
		},
		"GeneralResult": func(infoRes InfoRes) Result {
			if infoRes.Running {
				if infoRes.Res == nil {
					return ResultUnk
				}
				return infoRes.Res.Result
			} else {
				if infoRes.Res == nil {
					return ResultUnfinished
				} else if infoRes.Res.Result == ResultUnk {
					return ResultUnfinished
				}
				return infoRes.Res.Result
			}
		},
		"Decorate": func(data interface{}, deco interface{}) *PipelineDecorator {
			return &PipelineDecorator{
				Data: data,
				Deco: deco,
			}
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

	log.Infof("Listening on http://%s", listenAddr)
	router.Run(listenAddr)
}
