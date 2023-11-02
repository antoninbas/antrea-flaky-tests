package main

import (
	"archive/zip"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v54/github"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/afero"
	"golang.org/x/oauth2"
)

const (
	owner = "antrea-io"
	repo  = "antrea"

	defaultNumWorkers = 10
	// logs are not saved beyond 90 days
	defaultMaxAgeDays = 89
)

var (
	numWorkers int
	maxAgeDays int
)

var testFailureRe = regexp.MustCompile(`--- FAIL: ([^\ ]+)`)

var FS = afero.NewMemMapFs()

var logger *slog.Logger

func downloadLogs(ctx context.Context, logsURL *url.URL, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", logsURL.String(), nil)
	if err != nil {
		return 0, err
	}
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

func getFailedTests(b []byte) []string {
	g := testFailureRe.FindAllSubmatch(b, -1)
	tests := make([]string, len(g))
	for idx := range g {
		tests[idx] = string(g[idx][1])
	}
	return tests
}

func processFailedRunAttempt(ctx context.Context, logger *slog.Logger, client *github.Client, run *github.WorkflowRun, failedTestsCh chan<- []string) error {
	logsURL, _, err := client.Actions.GetWorkflowRunAttemptLogs(ctx, owner, repo, *run.ID, 1, true)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s.logs.zip", *run.ID)
	f, err := FS.Create(name)
	if err != nil {
		return err
	}
	defer FS.Remove(name)
	size, err := downloadLogs(ctx, logsURL, f)
	if err != nil {
		return err
	}
	logger.DebugContext(ctx, "Downloaded logs")
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r, err := zip.NewReader(f, size)
	if err != nil {
		return err
	}
	fileFilter := func(name string) bool {
		// ignore files in directories
		if filepath.Base(name) != name {
			return false
		}
		return strings.Contains(name, "E2e tests on a Kind cluster") || strings.Contains(name, "NetworkPolicy conformance tests on a Kind cluster")
	}
	count := 0
	for _, f := range r.File {
		if !fileFilter(f.Name) {
			continue
		}
		logger.DebugContext(ctx, "Found file", "name", f.Name)
		if err := func() error {
			rc, err := f.Open()
			defer rc.Close()
			if err != nil {
				return fmt.Errorf("failed to open file: %w", err)
			}
			b, err := io.ReadAll(rc)
			if err != nil {
				return fmt.Errorf("failed to read all file contents: %w", err)
			}
			failedTests := getFailedTests(b)
			logger.DebugContext(ctx, "Failed tests", "tests", failedTests)
			count += len(failedTests)
			failedTestsCh <- failedTests
			return nil
		}(); err != nil {
			logger.ErrorContext(ctx, "Error when processing file, skipping", "name", f.Name, "error", err)
		}
	}
	if count == 0 {
		logger.DebugContext(ctx, "No failed test found for failed workflow, failures may not have been caused by E2e tests")
	}
	return nil
}

func processFailedRunAttempts(ctx context.Context, client *github.Client, failedAttemptsCh <-chan *github.WorkflowRun, failedTestsCh chan<- []string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case run, ok := <-failedAttemptsCh:
			if !ok {
				return nil
			}
			logger := logger.With("runID", *run.ID, "attempt", *run.RunAttempt)
			logger.DebugContext(ctx, "Processing failed run attempt")
			if err := processFailedRunAttempt(ctx, logger, client, run, failedTestsCh); err != nil {
				logger.ErrorContext(ctx, "Failed to process failed run attempt", "error", err)
			}
		}
	}
}

func processFailedTests(ctx context.Context, failedTestsCh <-chan []string) (map[string]int64, error) {
	failedTests := make(map[string]int64)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case tests, ok := <-failedTestsCh:
			if !ok {
				return failedTests, nil
			}
			for _, t := range tests {
				failedTests[t] += 1
			}
		}
	}
}

type stats struct {
	successCount int64
	failureCount int64
}

func listWorkflows(ctx context.Context, client *github.Client, from time.Time, failedAttemptsCh chan<- *github.WorkflowRun) stats {
	s := stats{}
	options := &github.ListWorkflowRunsOptions{
		Branch:              "main",
		Event:               "push",
		Created:             fmt.Sprintf(">=%s", from.Format(time.DateOnly)),
		ExcludePullRequests: true,
		ListOptions: github.ListOptions{
			Page:    0,
			PerPage: 100,
		},
	}
	for {
		runs, resp, err := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, "kind.yml", options)
		if err != nil {
			logger.ErrorContext(ctx, "ListWorkflowRunsByFileName failed", "error", err)
			break
		}
		for _, run := range runs.WorkflowRuns {
			logger := logger.With("runID", *run.ID)
			logger.DebugContext(ctx, "Processing workflow run", "numAttempts", *run.RunAttempt)
			for {
				if *run.Status == "completed" {
					if *run.Conclusion == "success" {
						s.successCount++
					} else if *run.Conclusion == "failure" {
						s.failureCount++
						failedAttemptsCh <- run
					}
				}
				if *run.RunAttempt <= 1 {
					break
				}
				logger.DebugContext(ctx, "There are other attempts for the workflow run")
				run, _, err = client.Actions.GetWorkflowRunAttempt(ctx, owner, repo, *run.ID, *run.RunAttempt-1, &github.WorkflowRunAttemptOptions{
					ExcludePullRequests: &options.ExcludePullRequests,
				})
				if err != nil {
					logger.ErrorContext(ctx, "GetWorkflowRunAttempt failed", "attempt", *run.RunAttempt-1, "error", err)
					break
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		options.Page = resp.NextPage
	}
	return s
}

func collectTestFailures(ctx context.Context, client *github.Client) (stats, map[string]int64) {
	failedAttemptsCh := make(chan *github.WorkflowRun, numWorkers*200)
	failedTestsCh := make(chan []string, numWorkers)

	var wgRuns sync.WaitGroup
	logger.InfoContext(ctx, "Starting workers to process failed run results", "num", numWorkers)
	for i := 0; i < numWorkers; i++ {
		i := i
		wgRuns.Add(1)
		go func() {
			defer wgRuns.Done()
			err := processFailedRunAttempts(ctx, client, failedAttemptsCh, failedTestsCh)
			if err != nil {
				logger.ErrorContext(ctx, "Error when processing runs", "worker", i, "error", err)
			}
		}()
	}

	var s stats
	go func() {
		from := time.Now().Add(time.Duration(-maxAgeDays*24) * time.Hour)
		s = listWorkflows(ctx, client, from, failedAttemptsCh)

		logger.InfoContext(ctx, "Collected runs", "success", s.successCount, "failure", s.failureCount)
		close(failedAttemptsCh)
		logger.InfoContext(ctx, "Waiting for workers to complete...")
		wgRuns.Wait()
		close(failedTestsCh)
	}()

	failedTests, err := processFailedTests(ctx, failedTestsCh)
	if err != nil {
		logger.ErrorContext(ctx, "Error when processing failed tests", "error", err)
	}

	return s, failedTests
}

func showResults(s stats, failedTests map[string]int64) {
	count := s.successCount + s.failureCount

	func() {
		table := tablewriter.NewWriter(os.Stdout)
		table.SetCaption(true, "Summary")
		table.SetHeader([]string{"Successful Attempts", "Failed Attempts", "Failure Rate"})
		table.Append([]string{fmt.Sprintf("%d", s.successCount), fmt.Sprintf("%d", s.failureCount), fmt.Sprintf("%f", float64(s.failureCount)/float64(count))})
		table.Render()
	}()

	func() {
		maxNesting := 1
		sortedTestNames := make([]string, 0, len(failedTests))
		for name, _ := range failedTests {
			maxNesting = max(maxNesting, strings.Count(name, "/")+1)
			sortedTestNames = append(sortedTestNames, name)
		}
		slices.Sort(sortedTestNames)
		table := tablewriter.NewWriter(os.Stdout)
		table.SetCaption(true, "All failures")
		indexes := make([]int, maxNesting)
		for i := range indexes {
			indexes[i] = i
		}
		table.SetAutoMergeCellsByColumnIndex(indexes)
		table.SetRowLine(true)
		header := []string{"Test Name"}
		for i := 1; i < maxNesting; i++ {
			header = append(header, "")
		}
		header = append(header, "Failures")
		header = append(header, "Failure rate")
		table.SetHeader(header)
		row := make([]string, maxNesting+2)
		for _, name := range sortedTestNames {
			fragments := strings.Split(name, "/")
			i := 0
			for i = range fragments {
				row[i] = fragments[i]
			}
			for i = i + 1; i < maxNesting; i++ {
				row[i] = ""
			}
			failures := failedTests[name]
			row[i] = fmt.Sprintf("%d", failures)
			i++
			row[i] = fmt.Sprintf("%f", float64(failures)/float64(count))
			table.Append(row)
		}
		table.Render()
	}()

	func() {
		table := tablewriter.NewWriter(os.Stdout)
		table.SetCaption(true, "Top 10 failures")
		table.SetHeader([]string{"Test Name", "Failures", "Failure Rate"})
		type pair struct {
			name     string
			failures int64
		}
		pairs := make([]pair, 0, len(failedTests))
		for name, failures := range failedTests {
			pairs = append(pairs, pair{
				name:     name,
				failures: failures,
			})
		}
		slices.SortFunc(pairs, func(p1, p2 pair) int {
			if p1.failures > p2.failures {
				return -1
			}
			if p1.failures < p2.failures {
				return 1
			}
			return strings.Compare(p1.name, p2.name)
		})
		row := make([]string, 3)
		for i := 0; i < 10; i++ {
			if i >= len(pairs) {
				break
			}
			p := &pairs[i]
			row[0] = p.name
			row[1] = fmt.Sprintf("%d", p.failures)
			row[2] = fmt.Sprintf("%f", float64(p.failures)/float64(count))
			table.Append(row)
		}
		table.Render()
	}()
}

func main() {
	ctx := context.Background()

	var debug bool
	flag.IntVar(&numWorkers, "num-workers", defaultNumWorkers, "Number of concurrent workers downloading and processing workflow run logs")
	flag.IntVar(&maxAgeDays, "max-age", defaultMaxAgeDays, "Max age in days of workflow runs to consider")
	flag.BoolVar(&debug, "debug", false, "Include debug logs")
	flag.Parse()

	logOptions := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	if debug {
		logOptions.Level = slog.LevelDebug
	}
	logHandler := slog.NewTextHandler(os.Stderr, logOptions)
	logger = slog.New(logHandler)

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		logger.Error("Missing GITHUB_TOKEN")
		os.Exit(1)
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{
			AccessToken: token,
		},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	s, failedTests := collectTestFailures(ctx, client)
	showResults(s, failedTests)
}
