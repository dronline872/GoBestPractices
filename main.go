package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"
)

type CrawlResult struct {
	Err   error
	Title string
	Url   string
}

type Page interface {
	GetTitle() string
	GetLinks() []string
}

type page struct {
	doc *goquery.Document
}

func NewPage(raw io.Reader) (*page, error) {
	doc, err := goquery.NewDocumentFromReader(raw)
	if err != nil {
		return nil, err
	}
	return &page{doc: doc}, nil
}

func (p *page) GetTitle() string {
	return p.doc.Find("title").First().Text()
}

func (p *page) GetLinks() []string {
	var urls []string
	p.doc.Find("a").Each(func(_ int, s *goquery.Selection) {
		url, ok := s.Attr("href")
		if ok {
			urls = append(urls, url)
		}
	})
	return urls
}

type Requester interface {
	Get(ctx context.Context, url string) (Page, error)
}

type requester struct {
	timeout time.Duration
}

func NewRequester(timeout time.Duration) requester {
	return requester{timeout: timeout}
}

func (r requester) Get(ctx context.Context, url string) (Page, error) {

	select {
	case <-ctx.Done():
		return nil, nil
	default:
		cl := &http.Client{
			Timeout: r.timeout,
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		body, err := cl.Do(req)
		if err != nil {
			return nil, err
		}
		defer body.Body.Close()
		page, err := NewPage(body.Body)
		if err != nil {
			return nil, err
		}
		return page, nil
	}

}

//Crawler - интерфейс (контракт) краулера
type Crawler interface {
	Scan(ctx context.Context, url string, depth int, domain string)
	ChanResult() <-chan CrawlResult
	AddDepth()
}

type crawler struct {
	r       Requester
	res     chan CrawlResult
	visited map[string]struct{}
	mu      sync.RWMutex
	depth   int
	logger  *zap.Logger
}

func NewCrawler(r Requester, logger *zap.Logger) *crawler {
	return &crawler{
		r:       r,
		res:     make(chan CrawlResult),
		visited: make(map[string]struct{}),
		mu:      sync.RWMutex{},
		logger:  logger,
	}
}

func (c *crawler) Scan(ctx context.Context, url string, depth int, domain string) {
	if c.depth > 0 {
		c.logger.Info("add depth",
			zap.Int("now depth", depth),
		)
		depth += c.depth
		c.mu.Lock()
		c.depth = 0
		c.mu.Unlock()
	}

	if depth <= 0 { //Проверяем то, что есть запас по глубине
		c.logger.Debug("depth <= 0",
			zap.Int("depth: ", depth),
		)
		return
	}
	c.mu.RLock()
	_, ok := c.visited[url] //Проверяем, что мы ещё не смотрели эту страницу
	c.mu.RUnlock()
	if ok {
		c.logger.Debug("page not viewed",
			zap.Int("depth: ", depth),
		)
		return
	}
	select {
	case <-ctx.Done(): //Если контекст завершен - прекращаем выполнение
		c.logger.Debug("Done")
		return
	default:
		page, err := c.r.Get(ctx, url) //Запрашиваем страницу через Requester
		if err != nil {
			c.res <- CrawlResult{Err: err} //Записываем ошибку в канал
			return
		}
		c.mu.Lock()
		c.logger.Info("New page", zap.String("Page: ", url))
		c.visited[url] = struct{}{} //Помечаем страницу просмотренной
		c.mu.Unlock()
		c.res <- CrawlResult{ //Отправляем результаты в канал
			Title: page.GetTitle(),
			Url:   url,
		}
		for _, link := range page.GetLinks() {
			hasHttp := strings.Contains(link, "http")
			if !hasHttp {
				link = domain + link
			}

			go c.Scan(ctx, link, depth-1, domain) //На все полученные ссылки запускаем новую рутину сборки
		}
	}
}

func (c *crawler) ChanResult() <-chan CrawlResult {
	return c.res
}

func (c *crawler) AddDepth() {
	c.mu.Lock()
	c.depth += 2
	c.mu.Unlock()
}

//Config - структура для конфигурации
type Config struct {
	MaxDepth   int
	MaxResults int
	MaxErrors  int
	Url        string
	Timeout    int //in seconds
	Logger     *zap.Logger
}

func main() {
	logger, _ := zap.NewProduction()
	cfg := Config{
		MaxDepth:   3,
		MaxResults: 10,
		MaxErrors:  5,
		Url:        "https://telegram.org",
		Timeout:    20,
	}
	var cr Crawler
	var r Requester

	r = NewRequester(time.Duration(cfg.Timeout) * time.Second)
	cr = NewCrawler(r, logger)

	ctx, cancel := context.WithCancel(context.Background())
	go cr.Scan(ctx, cfg.Url, cfg.MaxDepth, cfg.Url) //Запускаем краулер в отдельной рутине
	go processResult(ctx, cancel, cr, cfg, logger)  //Обрабатываем результаты в отдельной рутине

	sigCh := make(chan os.Signal, 1) //Создаем канал для приема сигналов
	signal.Notify(sigCh,
		syscall.SIGINT,
		syscall.SIGUSR1,
	) //Подписываемся на сигнал SIGINT
	for {
		select {
		case <-ctx.Done(): //Если всё завершили - выходим
			logger.Info("Done")
			return
		case signal := <-sigCh:
			if signal == syscall.SIGUSR1 {
				logger.Info("receiving the SIGUSR1 signal")
				cr.AddDepth()
			} else if signal == syscall.SIGINT {
				logger.Info("receiving the SIGINT signal")
				cancel() //Если пришёл сигнал SigInt - завершаем контекст
			}

		}
	}
}

func processResult(ctx context.Context, cancel func(), cr Crawler, cfg Config, logger *zap.Logger) {
	var maxResult, maxErrors = cfg.MaxResults, cfg.MaxErrors

	defer func() {
		if r := recover(); r != nil {
			logger.Panic(fmt.Sprintln("Panic error: ", r))
		}
	}()

	for {
		// panic("PANIC!!!")
		select {
		case <-ctx.Done():
			return
		case msg := <-cr.ChanResult():
			if msg.Err != nil {
				maxErrors--
				logger.Error(fmt.Sprintf("crawler result return err: %s\n", msg.Err.Error()))
				if maxErrors <= 0 {
					cancel()
					return
				}
			} else {
				maxResult--
				logger.Info("crawler result",
					zap.String("url", msg.Url),
					zap.String("title", msg.Title),
				)
				if maxResult <= 0 {
					cancel()
					return
				}
			}
		}
	}
}
