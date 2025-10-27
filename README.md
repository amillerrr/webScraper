# Web Scraper

A concurrent web scraper built in Go. I made this while preparing for backend engineering interviews to practice working with goroutines, channels, and production-level code patterns.

## What it does

Crawls websites using BFS (breadth-first search) and extracts page titles, links, and images. It handles all the stuff you'd need for a real scraper: URL normalization, robots.txt checking, rate limiting, timeouts, and concurrent workers.

## Features

- **URL Normalization** - Converts relative URLs to absolute and handles duplicates properly
- **robots.txt Support** - Checks and respects robots.txt before scraping
- **Rate Limiting** - Uses token bucket algorithm to limit requests per second
- **Concurrent Workers** - Worker pool pattern with configurable number of goroutines
- **Context Support** - Proper timeout and cancellation handling
- **Thread-Safe** - Uses sync.Map and channels for safe concurrent access

## Install

```bash
go get github.com/PuerkitoBio/goquery
go get github.com/google/uuid
go get github.com/temoto/robotstxt
go get golang.org/x/time/rate
```

## Usage

Basic example:

```go
storage := NewMemoryStorage()
scraper := NewScraper(
    2.0,                  // 2 requests per second
    storage,
    "MyBot/1.0",         // user agent
    30*time.Second,      // request timeout
    5,                   // number of workers
)

job := NewScrapeJob("https://example.com", 2, 20)
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()

if err := scraper.ProcessJob(ctx, job); err != nil {
    log.Fatal(err)
}

fmt.Printf("Scraped %d pages\n", len(job.Results))
```

## Testing

I included a test version that you can run on any site:

```bash
# Check for race conditions
go run -race test_scraper.go

# Normal run
go run test_scraper.go
```

The test version has better logging and shows you what's happening as it scrapes.

## Architecture

The worker pool pattern has three main parts:

- **Workers** (N goroutines) - Actually scrape pages concurrently
- **Coordinator** (1 goroutine) - Manages the BFS queue and sends URLs to workers
- **Collector** (1 goroutine) - Gathers results from workers

Communication happens through buffered channels. The coordinator and collector never compete for the same channel to avoid race conditions.

## Performance

With rate limiting at 1 request/second:
- 10 pages: ~10 seconds
- 50 pages: ~50 seconds
- 100 pages: ~100 seconds

Adding more workers helps utilize wait time but the rate limiter ensures you don't hammer servers.

## Notes

**Why sync.Map instead of regular map?**  
Multiple workers need to check if a URL was already visited. Regular maps aren't thread-safe, so sync.Map handles the concurrent access.

**Why separate channels for counting?**  
Early versions had the coordinator and collector both reading from resultChan, which caused a race condition. Now the collector is the only reader and signals the count separately.

**Why check robots.txt before rate limiting?**  
Saves rate limit tokens. If we're not allowed to scrape a URL anyway, no point wasting our quota on it.

## Known Limitations

- Can't execute JavaScript (only static HTML)
- No support for authentication
- Memory storage only (results lost on restart)
- Doesn't handle redirects to different domains well
- No retry logic for failed requests

These would be straightforward to add but I kept it focused on the core scraping logic and concurrency patterns.

