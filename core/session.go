package core

import (
	"context"
	"encoding/csv"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

type Session struct {
	sync.Mutex

	Version          string
	Log              *Logger
	Options          *Options
	Config           *Config
	Signatures       []Signature
	Repositories     chan GitResource
	Gists            chan string
	Comments         chan string
	Context          context.Context
	Clients          chan *GitHubClientWrapper
	ExhaustedClients chan *GitHubClientWrapper
	CsvWriter        *csv.Writer
}

func (s *Session) Start() {
	rand.Seed(time.Now().Unix())

	s.InitLogger()
	s.InitThreads()
	s.InitSignatures()
	s.InitGitHubClients()
	s.InitCsvWriter()
}

func (s *Session) InitLogger() {
	s.Log = &Logger{s: s}
	s.Log.SetDebug(*s.Options.Debug)
	s.Log.SetSilent(*s.Options.Silent)
}

func (s *Session) InitSignatures() {
	s.Signatures = GetSignatures(s)
}

func (s *Session) InitGitHubClients() {
	if len(*s.Options.Local) <= 0 {
		chanSize := *s.Options.Threads * (len(s.Config.GitHubAccessTokens) + 1)
		s.Clients = make(chan *GitHubClientWrapper, chanSize)
		s.ExhaustedClients = make(chan *GitHubClientWrapper, chanSize)
		for _, token := range s.Config.GitHubAccessTokens {
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(s.Context, ts)

			client := github.NewClient(tc)
			client.UserAgent = fmt.Sprintf("%s v%s", Name, Version)
			_, _, err := client.Users.Get(s.Context, "")

			if err != nil {
				if _, ok := err.(*github.ErrorResponse); ok {
					s.Log.Warn("Failed to validate token %s[..]: %s", token[:10], err)
					continue
				}
			}

			for i := 0; i <= *s.Options.Threads; i++ {
				s.Clients <- &GitHubClientWrapper{client, token, time.Now().Add(-1 * time.Second)}
			}
		}

		if len(s.Clients) < 1 {
			s.Log.Fatal("No valid GitHub tokens provided. Quitting!")
		}
	}
}

func (s *Session) GetClient() *GitHubClientWrapper {
	for {
		select {

		case client := <-s.Clients:
			s.Log.Debug("Using client with token: %s", client.Token[:10])
			return client

		case client := <-s.ExhaustedClients:
			sleepTime := time.Until(client.RateLimitedUntil)
			s.Log.Warn("All GitHub tokens exhausted/rate limited. Sleeping for %s", sleepTime.String())
			time.Sleep(sleepTime)
			s.Log.Debug("Returning client %s to pool", client.Token[:10])
			s.FreeClient(client)

		default:
			s.Log.Debug("Available Clients: %d", len(s.Clients))
			s.Log.Debug("Exhausted Clients: %d", len(s.ExhaustedClients))
			time.Sleep(time.Millisecond * 1000)
		}
	}
}

// FreeClient returns the GitHub Client to the pool of available,
// non-rate-limited channel of clients in the session
func (s *Session) FreeClient(client *GitHubClientWrapper) {
	if client.RateLimitedUntil.After(time.Now()) {
		s.ExhaustedClients <- client
	} else {
		s.Clients <- client
	}
}

func (s *Session) InitThreads() {
	if *s.Options.Threads == 0 {
		numCPUs := runtime.NumCPU()
		s.Options.Threads = &numCPUs
	}

	runtime.GOMAXPROCS(*s.Options.Threads + 1)
}

func (s *Session) InitCsvWriter() {
	if *s.Options.CSVPath == "" {
		return
	}

	writeHeader := false
	if !PathExists(*s.Options.CSVPath) {
		writeHeader = true
	}

	file, err := os.OpenFile(*s.Options.CSVPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		s.Log.Error("Could not create/open CSV file: %v", err)
	}

	s.CsvWriter = csv.NewWriter(file)

	if writeHeader {
		s.WriteToCsv([]string{"Repository name", "Signature name", "Matching file", "Matches"})
	}
}

func (s *Session) WriteToCsv(line []string) {
	if *s.Options.CSVPath == "" {
		return
	}

	s.CsvWriter.Write(line)
	s.CsvWriter.Flush()
}

func NewSession(ctx context.Context, o *Options) (*Session, error) {
	s := &Session{
		// TODO: Remove (contexts should not be embedded in structs)
		Context:      ctx,
		Repositories: make(chan GitResource, 1000),
		Gists:        make(chan string, 100),
		Comments:     make(chan string, 1000),
		Options:      &DefaultOptions,
	}

	s.Options.Merge(o)
	sc, err := ParseConfig(s.Options)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	s.Config = sc
	s.Start()
	return s, nil
}
