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

// fetch and cache robots.txt
func (rc *RobotsCache) getRobots(domain string) (*robotstxt.RobotsData, error) {
	// Check cache first
	rc.mu.RLock()
	if robots, exists := rc.cache[domain]; exists {
		rc.mu.RUnlock()
		return robots, nil
	}
	rc.mu.RUnlock()

	// Fetch if not in cache
	rc.mu.Lock()
	defer rc.mu.Unlock()

	// Double-check after acquiring write lock
	if robots, exists := rc.cache[domain]; exists {
		return robots, nil
	}

	robotsURL := fmt.Sprintf("%s/robots.txt", domain)
	resp, err := http.Get(robotsURL)
	if err != nil {
		// If robots.txt doesn't exist, assume everything is allowed
		log.Printf("No robots.txt found for %s, allowing all: %v", domain, err)
		robots := &robotstxt.RobotsData{}
		rc.cache[domain] = robots
		return robots, nil
	}
	defer resp.Body.Close()

	// Parse
	robots, err := robotstxt.FromResponse(resp)
	if err != nil {
		log.Printf("Failed to parse robots.txt for %s: %v", domain, err)
		robots = &robotstxt.RobotsData{} // Allow all on parse error
	}

	rc.cache[domain] = robots
	return robots, nil
}

// Check permission to fetch url
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

	// Check if our user agent is allowed to fetch this path
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
	// Parse URL
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	// Convert to absolute URLs
	if !parsed.IsAbs() {
		parsed = baseURL.ResolveReference(parsed)
	}

	// Remove fragments
	parsed.Fragment = ""

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)

	if parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}

	if (parsed.Scheme == "http" && strings.HasSuffix(parsed.Host, ":80")) || (parsed.Scheme == "https" && strings.HasSuffix(parsed.Host, ":443")) {
		parsed.Host = strings.Split(parsed.Host, ":")[0]
	}

	return parsed.String(), nil
}

func (s *Scraper) scrapePage(ctx context.Context, rawURL string) (ScrapedPage, error) {
	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		return ScrapedPage{}, ctx.Err()
	default:
	}

	// Check robots.txt
	if !s.robotsCache.CanFetch(rawURL, s.userAgent) {
		return ScrapedPage{}, fmt.Errorf("disallowed by robots.txt: %s", rawURL)
	}

	// rate limiter permission step
	if err := s.rateLimiter.Wait(ctx); err != nil {
		return ScrapedPage{}, fmt.Errorf("rate limiter wait failed: %w", err)
	}

	// Create request with User-Agent header
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

	// Extract Links
	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists {
			// Normalize url
			normalizedURL, err := normalizeURL(href, baseURL)
			if err != nil {
				log.Printf("Failed to normalize URL %s: %v", href, err)
				return
			}

			if strings.HasPrefix(normalizedURL, "http://") || strings.HasPrefix(normalizedURL, "https://") {
				page.Links = append(page.Links, normalizedURL)
			}
		}
	})

	// Extract images
	doc.Find("img[src]").Each(func(i int, s *goquery.Selection) {
		if src, exists := s.Attr("src"); exists {
			normalizedURL, err := normalizeURL(src, baseURL)
			if err != nil {
				log.Printf("Failed to normalize image URL %s: %v", src, err)
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

			// Check if visited
			if _, visited := s.visited.LoadOrStore(urlDepth.URL, true); visited {
				continue
			}

			// Scrape the page
			page, err := s.scrapePage(ctx, urlDepth.URL)
			if err != nil {
				log.Printf("Worker error scraping %s: %v", urlDepth.URL, err)
				continue
			}

			// Send result
			select {
			case resultChan <- page:
			case <-ctx.Done():
				return
			}

			// Send discovered links with depth
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

	// Channels for coordinating workers
	urlChan := make(chan URLDepth, 100)       // Buffer to avoid blocking
	resultChan := make(chan ScrapedPage, 100) // Buffer for results
	linksChan := make(chan []URLDepth, 100)   // Buffer for discovered links

	var wg sync.WaitGroup

	// Start worker pool
	for i := 0; i < s.numWorkers; i++ {
		wg.Add(1)
		go s.worker(ctx, urlChan, resultChan, linksChan, &wg)
	}

	results := []ScrapedPage{}
	resultCountChan := make(chan int, 100) // FIX: Separate channel for counting
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
				// FIX: Signal coordinator that we got a result
				select {
				case resultCountChan <- len(results):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// manage BFS queue
	coordinatorDone := make(chan struct{})
	go func() {
		defer close(coordinatorDone)
		defer close(resultCountChan)

		toVisit := []URLDepth{{URL: normalizedStartURL, Depth: 0}}
		pagesScraped := 0

		// Send initial URL
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
				// Add discovered links to queue if within depth limit
				for _, link := range discoveredLinks {
					if link.Depth <= job.Depth {
						toVisit = append(toVisit, link)
					}
				}

			case count := <-resultCountChan:
				pagesScraped = count

				// Send next URL to workers if available
				if len(toVisit) > 0 && pagesScraped < job.MaxPages {
					select {
					case urlChan <- toVisit[0]:
						toVisit = toVisit[1:]
					case <-ctx.Done():
						return
					}
				}

				// Stop if limits reached
				if pagesScraped >= job.MaxPages || len(toVisit) == 0 {
					return
				}
			}
		}
	}()

	// Wait for coordinator to finish
	<-coordinatorDone

	// Close URL channel to signal workers to stop
	close(urlChan)

	// Wait for all workers to finish
	wg.Wait()

	// Close channels
	close(resultChan)
	close(linksChan)

	// Wait for collector to finish
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

func main() {
	storage := NewMemoryStorage()
	userAgent := "WebScraper/1.0 (+https://example.com/bot)"
	requestTimeout := 30 * time.Second
	numWorkers := 5

	scraper := NewScraper(2.0, storage, userAgent, requestTimeout, numWorkers)

	job := NewScrapeJob("https://example.com", 2, 10)
	fmt.Printf("Created job %s with %d workers\n", job.ID, numWorkers)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Process job with context
	startTime := time.Now()
	if err := scraper.ProcessJob(ctx, job); err != nil {
		if err == context.DeadlineExceeded {
			log.Printf("Job %s timed out after 5 minutes", job.ID)
		} else if err == context.Canceled {
			log.Printf("Job %s was cancelled", job.ID)
		} else {
			log.Fatal(err)
		}
	}
	duration := time.Since(startTime)

	fmt.Printf("Job %s: scraped %d pages in %v\n", job.ID, len(job.Results), duration)
}
