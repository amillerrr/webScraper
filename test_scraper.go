package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"github.com/temoto/robotstxt"
	"golang.org/x/time/rate"
)

type ScrapeJob struct {
	ID          string
	URL         string
	Depth       int
	MaxPages    int
	Status      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	Results     []ScrapedPage
}

type ScrapedPage struct {
	URL       string
	Title     string
	Links     []string
	Images    []string
	ScrapedAt time.Time
}

type URLDepth struct {
	URL   string
	Depth int
}

type Storage interface {
	SaveJob(job *ScrapeJob) error
	GetJob(id string) (*ScrapeJob, error)
	ListJobs() ([]*ScrapeJob, error)
}

type MemoryStorage struct {
	mu   sync.RWMutex
	jobs map[string]*ScrapeJob
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		jobs: make(map[string]*ScrapeJob),
	}
}

func (m *MemoryStorage) SaveJob(job *ScrapeJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = job
	return nil
}

func (m *MemoryStorage) GetJob(id string) (*ScrapeJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, exists := m.jobs[id]
	if !exists {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return job, nil
}

func (m *MemoryStorage) ListJobs() ([]*ScrapeJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := make([]*ScrapeJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	return jobs, nil
}

type RobotsCache struct {
	mu    sync.RWMutex
	cache map[string]*robotstxt.RobotsData
}

func NewRobotsCache() *RobotsCache {
	return &RobotsCache{
		cache: make(map[string]*robotstxt.RobotsData),
	}
}

func (rc *RobotsCache) getRobots(domain string) (*robotstxt.RobotsData, error) {
	rc.mu.RLock()
	if robots, exists := rc.cache[domain]; exists {
		rc.mu.RUnlock()
		return robots, nil
	}
	rc.mu.RUnlock()

	rc.mu.Lock()
	defer rc.mu.Unlock()

	if robots, exists := rc.cache[domain]; exists {
		return robots, nil
	}

	robotsURL := fmt.Sprintf("%s/robots.txt", domain)
	resp, err := http.Get(robotsURL)
	if err != nil {
		log.Printf("No robots.txt found for %s, allowing all: %v", domain, err)
		robots := &robotstxt.RobotsData{}
		rc.cache[domain] = robots
		return robots, nil
	}
	defer resp.Body.Close()

	robots, err := robotstxt.FromResponse(resp)
	if err != nil {
		log.Printf("Failed to parse robots.txt for %s: %v", domain, err)
		robots = &robotstxt.RobotsData{}
	}

	rc.cache[domain] = robots
	return robots, nil
}

func (rc *RobotsCache) CanFetch(urlStr string, userAgent string) bool {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return false
	}

	domain := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	robots, err := rc.getRobots(domain)
	if err != nil {
		return false
	}

	group := robots.FindGroup(userAgent)
	return group.Test(parsed.Path)
}

type Scraper struct {
	visited        sync.Map
	rateLimiter    *rate.Limiter
	storage        Storage
	robotsCache    *RobotsCache
	userAgent      string
	requestTimeout time.Duration
	httpClient     *http.Client
	numWorkers     int
}

func NewScraper(requestsPerSecond float64, storage Storage, userAgent string, requestTimeout time.Duration, numWorkers int) *Scraper {
	return &Scraper{
		rateLimiter:    rate.NewLimiter(rate.Limit(requestsPerSecond), 1),
		storage:        storage,
		robotsCache:    NewRobotsCache(),
		userAgent:      userAgent,
		requestTimeout: requestTimeout,
		numWorkers:     numWorkers,
		httpClient: &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

func normalizeURL(rawURL string, baseURL *url.URL) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	if !parsed.IsAbs() {
		parsed = baseURL.ResolveReference(parsed)
	}

	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)

	if parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}

	if (parsed.Scheme == "http" && strings.HasSuffix(parsed.Host, ":80")) || 
	   (parsed.Scheme == "https" && strings.HasSuffix(parsed.Host, ":443")) {
		parsed.Host = strings.Split(parsed.Host, ":")[0]
	}

	return parsed.String(), nil
}

func (s *Scraper) scrapePage(ctx context.Context, rawURL string) (ScrapedPage, error) {
	select {
	case <-ctx.Done():
		return ScrapedPage{}, ctx.Err()
	default:
	}

	if !s.robotsCache.CanFetch(rawURL, s.userAgent) {
		return ScrapedPage{}, fmt.Errorf("disallowed by robots.txt: %s", rawURL)
	}

	if err := s.rateLimiter.Wait(ctx); err != nil {
		return ScrapedPage{}, fmt.Errorf("rate limiter wait failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return ScrapedPage{}, err
	}
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ScrapedPage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ScrapedPage{}, fmt.Errorf("bad status code: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return ScrapedPage{}, err
	}

	baseURL, err := url.Parse(rawURL)
	if err != nil {
		return ScrapedPage{}, err
	}

	page := ScrapedPage{
		URL:       rawURL,
		Title:     doc.Find("title").Text(),
		ScrapedAt: time.Now(),
		Links:     []string{},
		Images:    []string{},
	}

	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists {
			normalizedURL, err := normalizeURL(href, baseURL)
			if err != nil {
				return
			}

			if strings.HasPrefix(normalizedURL, "http://") || strings.HasPrefix(normalizedURL, "https://") {
				page.Links = append(page.Links, normalizedURL)
			}
		}
	})

	doc.Find("img[src]").Each(func(i int, s *goquery.Selection) {
		if src, exists := s.Attr("src"); exists {
			normalizedURL, err := normalizeURL(src, baseURL)
			if err != nil {
				return
			}
			page.Images = append(page.Images, normalizedURL)
		}
	})

	return page, nil
}

func (s *Scraper) worker(ctx context.Context, urlChan <-chan URLDepth, resultChan chan<- ScrapedPage, linksChan chan<- []URLDepth, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case urlDepth, ok := <-urlChan:
			if !ok {
				return
			}

			if _, visited := s.visited.LoadOrStore(urlDepth.URL, true); visited {
				continue
			}

			page, err := s.scrapePage(ctx, urlDepth.URL)
			if err != nil {
				log.Printf("Worker error scraping %s: %v", urlDepth.URL, err)
				continue
			}

			select {
			case resultChan <- page:
			case <-ctx.Done():
				return
			}

			discoveredLinks := make([]URLDepth, 0, len(page.Links))
			for _, link := range page.Links {
				discoveredLinks = append(discoveredLinks, URLDepth{
					URL:   link,
					Depth: urlDepth.Depth + 1,
				})
			}

			if len(discoveredLinks) > 0 {
				select {
				case linksChan <- discoveredLinks:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (s *Scraper) ProcessJob(ctx context.Context, job *ScrapeJob) error {
	job.Status = "running"

	parsedStartURL, err := url.Parse(job.URL)
	if err != nil {
		return fmt.Errorf("invalid start URL: %w", err)
	}
	normalizedStartURL, err := normalizeURL(job.URL, parsedStartURL)
	if err != nil {
		return fmt.Errorf("failed to normalize start URL: %w", err)
	}

	urlChan := make(chan URLDepth, 100)
	resultChan := make(chan ScrapedPage, 100)
	linksChan := make(chan []URLDepth, 100)

	var wg sync.WaitGroup

	for i := 0; i < s.numWorkers; i++ {
		wg.Add(1)
		go s.worker(ctx, urlChan, resultChan, linksChan, &wg)
	}

	results := []ScrapedPage{}
	resultCountChan := make(chan int, 100)
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for {
			select {
			case <-ctx.Done():
				return
			case page, ok := <-resultChan:
				if !ok {
					return
				}
				results = append(results, page)
				select {
				case resultCountChan <- len(results):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	coordinatorDone := make(chan struct{})
	go func() {
		defer close(coordinatorDone)

		toVisit := []URLDepth{{URL: normalizedStartURL, Depth: 0}}
		pagesScraped := 0

		select {
		case urlChan <- toVisit[0]:
			toVisit = toVisit[1:]
		case <-ctx.Done():
			return
		}

		for pagesScraped < job.MaxPages {
			select {
			case <-ctx.Done():
				return

			case discoveredLinks := <-linksChan:
				for _, link := range discoveredLinks {
					if link.Depth <= job.Depth {
						toVisit = append(toVisit, link)
					}
				}

			case count := <-resultCountChan:
				pagesScraped = count

				if len(toVisit) > 0 && pagesScraped < job.MaxPages {
					select {
					case urlChan <- toVisit[0]:
						toVisit = toVisit[1:]
					case <-ctx.Done():
						return
					}
				}

				if pagesScraped >= job.MaxPages || len(toVisit) == 0 {
					return
				}
			}
		}
	}()

	<-coordinatorDone
	close(urlChan)
	wg.Wait()
	close(resultChan)
	close(linksChan)
	<-collectorDone

	job.Results = results
	job.Status = "completed"
	now := time.Now()
	job.CompletedAt = &now

	return s.storage.SaveJob(job)
}

func NewScrapeJob(url string, depth int, maxPages int) *ScrapeJob {
	return &ScrapeJob{
		ID:        uuid.New().String(),
		URL:       url,
		Depth:     depth,
		MaxPages:  maxPages,
		Status:    "pending",
		CreatedAt: time.Now(),
		Results:   []ScrapedPage{},
	}
}

// Helper function to print results nicely
func printResults(job *ScrapeJob) {
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Printf("Job ID: %s\n", job.ID)
	fmt.Printf("Status: %s\n", job.Status)
	fmt.Printf("Started: %s\n", job.CreatedAt.Format(time.RFC3339))
	if job.CompletedAt != nil {
		fmt.Printf("Completed: %s\n", job.CompletedAt.Format(time.RFC3339))
		fmt.Printf("Duration: %v\n", job.CompletedAt.Sub(job.CreatedAt))
	}
	fmt.Printf("Pages Scraped: %d / %d\n", len(job.Results), job.MaxPages)
	fmt.Println(strings.Repeat("=", 80))

	for i, page := range job.Results {
		fmt.Printf("\n[%d] %s\n", i+1, page.URL)
		fmt.Printf("    Title: %s\n", strings.TrimSpace(page.Title))
		fmt.Printf("    Links found: %d\n", len(page.Links))
		fmt.Printf("    Images found: %d\n", len(page.Images))
		fmt.Printf("    Scraped at: %s\n", page.ScrapedAt.Format("15:04:05"))
		
		// Show first 5 links as sample
		if len(page.Links) > 0 {
			fmt.Println("    Sample links:")
			for j, link := range page.Links {
				if j >= 5 {
					fmt.Printf("    ... and %d more\n", len(page.Links)-5)
					break
				}
				fmt.Printf("      - %s\n", link)
			}
		}
	}
	
	fmt.Println("\n" + strings.Repeat("=", 80))
	
	// Summary statistics
	totalLinks := 0
	totalImages := 0
	uniqueDomains := make(map[string]bool)
	
	for _, page := range job.Results {
		totalLinks += len(page.Links)
		totalImages += len(page.Images)
		
		if parsed, err := url.Parse(page.URL); err == nil {
			uniqueDomains[parsed.Host] = true
		}
	}
	
	fmt.Println("\nSummary:")
	fmt.Printf("  Total links discovered: %d\n", totalLinks)
	fmt.Printf("  Total images found: %d\n", totalImages)
	fmt.Printf("  Unique domains: %d\n", len(uniqueDomains))
	fmt.Printf("  Average links per page: %.1f\n", float64(totalLinks)/float64(len(job.Results)))
	fmt.Println(strings.Repeat("=", 80))
}

func main() {
	storage := NewMemoryStorage()
	userAgent := "PersonalTestBot/1.0 (+https://andrew.mill3r.la; testing own site)"
	requestTimeout := 30 * time.Second
	numWorkers := 3 // Start with 3 workers for testing

	scraper := NewScraper(1.0, storage, userAgent, requestTimeout, numWorkers)

	// Test your personal website
	startURL := "https://andrew.mill3r.la"
	depth := 2        // Crawl up to 2 levels deep
	maxPages := 20    // Limit to 20 pages for initial test

	fmt.Printf("Starting scraper test on: %s\n", startURL)
	fmt.Printf("Configuration:\n")
	fmt.Printf("  - Depth: %d levels\n", depth)
	fmt.Printf("  - Max pages: %d\n", maxPages)
	fmt.Printf("  - Workers: %d\n", numWorkers)
	fmt.Printf("  - Rate: 1 request/second\n")
	fmt.Printf("  - Timeout: %v per request\n", requestTimeout)
	fmt.Println(strings.Repeat("=", 80))

	job := NewScrapeJob(startURL, depth, maxPages)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	startTime := time.Now()
	if err := scraper.ProcessJob(ctx, job); err != nil {
		if err == context.DeadlineExceeded {
			log.Printf("Job timed out after 5 minutes")
		} else if err == context.Canceled {
			log.Printf("Job was cancelled")
		} else {
			log.Fatalf("Error: %v", err)
		}
	}
	duration := time.Since(startTime)

	fmt.Printf("\nScraping completed in %v\n", duration)
	
	// Print detailed results
	printResults(job)
}
