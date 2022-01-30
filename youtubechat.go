package main

import (
	"context"
	"encoding/gob"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	youtube "google.golang.org/api/youtube/v3"
)

type LiveChatMessage struct {
	Author string
	Text   string
}

func createYoutubeClient() (service *youtube.Service, liveChatID string, err error) {
	secret, err := ioutil.ReadFile(*secretFile)
	if err != nil {
		return nil, "", fmt.Errorf("reading secret file: %v", err)
	}

	config := &oauth2.Config{
		ClientID:     *clientID,
		ClientSecret: strings.TrimSpace(string(secret)),
		Endpoint:     google.Endpoint,
		Scopes:       []string{youtube.YoutubeScope},
	}

	cl := newOAuthClient(context.Background(), config)

	service, err = youtube.New(cl)
	if err != nil {
		return nil, "", fmt.Errorf("creating YouTube service: %v", err)
	}

	resp, err := service.LiveBroadcasts.List(nil).BroadcastStatus("active").Do()
	if err != nil {
		return nil, "", fmt.Errorf("list live broadcasts: %v", err)
	}

	if len(resp.Items) != 1 {
		return nil, "", fmt.Errorf("unexpected number of live broadcasts: %d, want 1", len(resp.Items))
	}

	return service, resp.Items[0].Snippet.LiveChatId, nil
}

func fetchMsgs(ctx context.Context, service *youtube.Service, liveChatId, nextPageToken string) (res []*LiveChatMessage, nextToken string, sleepInterval time.Duration, err error) {
	if service == nil {
		res = append(res, &LiveChatMessage{
			Author: "Error",
			Text:   fmt.Sprintf("Look at logs comrade %d", time.Now().Unix()),
		})
		return res, "", time.Second * 10, nil
	}

	req := service.LiveChatMessages.
		List(liveChatId, []string{"id", "snippet", "authorDetails"}).
		Context(ctx)

	if nextPageToken != "" {
		req = req.PageToken(nextPageToken)
	}

	msgs, err := req.Do()
	if err != nil {
		return nil, "", 0, fmt.Errorf("listing live chat messages: %v", err)
	}

	for _, m := range msgs.Items {
		res = append(res, &LiveChatMessage{
			Author: m.AuthorDetails.DisplayName,
			Text:   m.Snippet.DisplayMessage,
		})
	}

	return res, msgs.NextPageToken, time.Duration(msgs.PollingIntervalMillis) * time.Millisecond, nil
}

func osUserCacheDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Caches")
	case "linux", "freebsd":
		return filepath.Join(os.Getenv("HOME"), ".cache")
	}
	log.Printf("TODO: osUserCacheDir on GOOS %q", runtime.GOOS)
	return "."
}

func tokenCacheFile(config *oauth2.Config) string {
	hash := fnv.New32a()
	hash.Write([]byte(config.ClientID))
	hash.Write([]byte(config.ClientSecret))
	hash.Write([]byte(strings.Join(config.Scopes, " ")))
	fn := fmt.Sprintf("go-api-demo-tok%v", hash.Sum32())
	return filepath.Join(osUserCacheDir(), url.QueryEscape(fn))
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := new(oauth2.Token)
	err = gob.NewDecoder(f).Decode(t)
	return t, err
}

func saveToken(file string, token *oauth2.Token) {
	f, err := os.Create(file)
	if err != nil {
		log.Printf("Warning: failed to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	gob.NewEncoder(f).Encode(token)
}

func newOAuthClient(ctx context.Context, config *oauth2.Config) *http.Client {
	cacheFile := tokenCacheFile(config)
	token, err := tokenFromFile(cacheFile)
	if err != nil {
		token = tokenFromWeb(ctx, config)
		saveToken(cacheFile, token)
	} else {
		log.Printf("Using cached token from %q", cacheFile)
	}

	return config.Client(ctx, token)
}

func tokenFromWeb(ctx context.Context, config *oauth2.Config) *oauth2.Token {
	ch := make(chan string)
	randState := fmt.Sprintf("st%d", time.Now().UnixNano())
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/favicon.ico" {
			http.Error(rw, "", 404)
			return
		}
		if req.FormValue("state") != randState {
			log.Printf("State doesn't match: req = %#v", req)
			http.Error(rw, "", 500)
			return
		}
		if code := req.FormValue("code"); code != "" {
			fmt.Fprintf(rw, "<h1>Success</h1>Authorized.")
			rw.(http.Flusher).Flush()
			ch <- code
			return
		}
		log.Printf("no code")
		http.Error(rw, "", 500)
	}))
	defer ts.Close()

	config.RedirectURL = ts.URL
	authURL := config.AuthCodeURL(randState)
	go openURL(authURL)
	log.Printf("Authorize this app at: %s", authURL)
	code := <-ch
	log.Printf("Got code: %s", code)

	token, err := config.Exchange(ctx, code)
	if err != nil {
		log.Fatalf("Token exchange error: %v", err)
	}
	return token
}

func openURL(url string) {
	try := []string{"xdg-open", "google-chrome", "open"}
	for _, bin := range try {
		err := exec.Command(bin, url).Run()
		if err == nil {
			return
		}
	}
	log.Printf("Error opening URL in browser.")
}

func valueOrFileContents(value string, filename string) string {
	if value != "" {
		return value
	}
	slurp, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatalf("Error reading %q: %v", filename, err)
	}
	return strings.TrimSpace(string(slurp))
}
