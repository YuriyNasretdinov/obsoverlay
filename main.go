package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mxmCherry/translit/ruicao"
	"github.com/pemistahl/lingua-go"
	"golang.org/x/net/websocket"
	"golang.org/x/text/transform"
	"google.golang.org/api/youtube/v3"
)

const savedFilePath = "/tmp/saved"

var timeout = flag.Duration("timeout", time.Second*20, "timeout for tests")
var clientID = flag.String("client-id", "", "Youtube API client ID")
var secretFile = flag.String("secret-file", "", "File that contains Youtube API secret")

type eventHandler struct {
	currentConnections int32

	service     *youtube.Service
	liveChatID  string
	detector    lingua.LanguageDetector
	transformer transform.Transformer
}

type YoutubeMessage struct {
	ChannelID string `json:"channel_id"`
	Avatar    string `json:"avatar"`
	Author    string `json:"author"`
	Text      string `json:"text"`
}

type Message struct {
	Running        bool            `json:"running,omitempty"`
	FatalError     string          `json:"fatalError,omitempty"`
	TestsError     string          `json:"testsError,omitempty"`
	TestsSuccess   bool            `json:"testsSuccess,omitempty"`
	TestsRace      bool            `json:"testsRace,omitempty"`
	YoutubeMessage *YoutubeMessage `json:"youtubeMessage,omitempty"`
}

func main() {
	flag.Parse()

	service, liveChatID, err := createYoutubeClient()
	if err != nil {
		log.Printf("Failed to create YouTube client: %v", err)
	}

	log.Printf("Live broadcast had ID %q", liveChatID)

	languages := []lingua.Language{
		lingua.English,
		lingua.French,
		lingua.German,
		lingua.Spanish,
		lingua.Russian,
	}

	detector := lingua.NewLanguageDetectorBuilder().
		FromLanguages(languages...).
		Build()

	h := &eventHandler{
		service:     service,
		liveChatID:  liveChatID,
		detector:    detector,
		transformer: ruicao.ToLatin().Transformer(),
	}

	http.HandleFunc("/", indexHandler)
	http.Handle("/events", websocket.Handler(h.HandleWebsocket))

	log.Printf("Running")
	log.Fatal(http.ListenAndServe(":80", nil))
}

func (h *eventHandler) HandleWebsocket(ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(ws.Request().Context())
	defer cancel()

	enc := json.NewEncoder(ws)

	currConn := atomic.AddInt32(&h.currentConnections, 1)
	defer atomic.AddInt32(&h.currentConnections, -1)

	if currConn > 1 {
		enc.Encode(Message{
			FatalError: fmt.Sprintf("too many connections: %v (max allowed is 1)", currConn),
		})
		return
	}

	go func() {
		ws.Read(make([]byte, 100))
		cancel()
	}()

	messages := make(chan Message)

	// Sending using select{} so that we don't block forever
	// if context is cancelled.
	send := func(m Message) {
		select {
		case <-ctx.Done():
			close(messages)
			return
		case messages <- m:
		}
	}

	go h.testResultsThread(ctx, send)
	go h.displayYoutubeChatThread(ctx, send)

	for m := range messages {
		enc.Encode(&m)
	}
}

func (h *eventHandler) displayYoutubeChatThread(ctx context.Context, send func(Message)) {
	nextPageToken := ""
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		res, nextPageTokenTmp, sleepInterval, err := fetchMsgs(ctx, h.service, h.liveChatID, nextPageToken)
		if err != nil {
			log.Printf("Error fetching live chat messages: %v", err)
			time.Sleep(time.Second * 10)
			continue
		}

		for _, m := range res {
			text := m.Text
			if lang, ok := h.detector.DetectLanguageOf(m.Text); ok && lang == lingua.Russian {
				translit, _, _ := transform.String(h.transformer, m.Text)
				if translit != "" {
					text = m.Text + " [" + translit + "]"
				}
			}

			send(Message{
				YoutubeMessage: &YoutubeMessage{
					ChannelID: m.ChannelID,
					Avatar:    m.Avatar,
					Author:    m.Author,
					Text:      text,
				},
			})

			h.onMessage(text)
		}

		nextPageToken = nextPageTokenTmp

		// looks like YouTube API limits are much lower than I thought.
		if minSleep := time.Second * 5; sleepInterval < minSleep {
			sleepInterval = minSleep
		}

		time.Sleep(sleepInterval)
	}
}

func (h *eventHandler) onMessage(text string) {
	switch strings.TrimSpace(text) {
	case "!test", "!tests", "!runtest", "!run", "!gotest", "!go test":
		fp, err := os.OpenFile(savedFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("Failed to create open file: %v", err)
			return
		}
		defer fp.Close()

		if _, err := fp.Write([]byte("1\n")); err != nil {
			log.Printf("Failed to write to saved file: %v", err)
			return
		}
	case "!testrace", "!test -race", "!tests -race", "!go test -race", "!gotest -race", "!runtest -race", "!runtest race":
		fp, err := os.OpenFile(savedFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("Failed to create open file: %v", err)
			return
		}
		defer fp.Close()

		if _, err := fp.Write([]byte("race\n")); err != nil {
			log.Printf("Failed to write to saved file: %v", err)
			return
		}
	}
}

func (h *eventHandler) testResultsThread(ctx context.Context, send func(Message)) {
	var lastSize int

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Millisecond * 100):
		}

		// I'm lazy so we'll read the whole file every 100ms :)
		contents, err := ioutil.ReadFile(savedFilePath)
		if err != nil {
			log.Printf("Failed to stat saved file: %v", err)
			time.Sleep(time.Second * 5)
			continue
		}

		if len(contents) <= lastSize {
			continue
		}

		race := bytes.Contains(contents[lastSize:], []byte("race"))
		send(Message{
			Running:   true,
			TestsRace: race,
		})

		lastSize = len(contents)
		err = runTest(race)
		log.Printf("runTest() result: %v", err)

		if err != nil {
			send(Message{
				TestsError: err.Error(),
				TestsRace:  race,
			})
		} else {
			send(Message{
				TestsSuccess: true,
				TestsRace:    race,
			})
		}
	}
}

//go:embed index.html
var indexHTML string

func indexHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", `text/html; charset="UTF-8"`)

	io.WriteString(w, indexHTML)
}

func runTest(race bool) error {
	tmpdir, err := os.MkdirTemp(os.TempDir(), "chukchatest")
	if err != nil {
		return fmt.Errorf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	log.Printf("Created temp dir %s", tmpdir)

	env := append([]string{}, os.Environ()...)
	for idx, e := range env {
		if strings.HasPrefix(e, "TMPDIR=") {
			env[idx] = "TMPDIR=" + tmpdir
		}
	}

	args := []string{"test", "-v"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "./...")

	cmd := exec.Command("go", args...)
	cmd.Dir = os.ExpandEnv("$HOME/chukcha")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting go test: %v", err)
	}

	// Kill all rogue processes left if any.
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("getting process group id: %v", err)
	}
	defer syscall.Kill(-pgid, syscall.SIGKILL)

	log.Printf("Process group id: %v", pgid)

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Wait() }()

	timeoutDuration := *timeout
	if race {
		// tests with -race take significantly longer
		timeoutDuration *= 5
	}

	select {
	case err := <-errCh:
		log.Printf("tests result: %v", err)
		if err != nil {
			return fmt.Errorf("error running tests: %v", err)
		}
	case <-time.After(timeoutDuration):
		return errors.New("tests timed out")
	}

	log.Printf("No errors executing tests")

	return nil
}
