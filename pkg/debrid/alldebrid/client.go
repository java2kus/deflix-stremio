package alldebrid

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/doingodswork/deflix-stremio/pkg/debrid"
)

type ClientOptions struct {
	BaseURL  string
	Timeout  time.Duration
	CacheAge time.Duration
}

func NewClientOpts(baseURL string, timeout, cacheAge time.Duration, extraHeaders []string) ClientOptions {
	return ClientOptions{
		BaseURL:  baseURL,
		Timeout:  timeout,
		CacheAge: cacheAge,
	}
}

var DefaultClientOpts = ClientOptions{
	BaseURL:  "https://api.alldebrid.com",
	Timeout:  5 * time.Second,
	CacheAge: 24 * time.Hour,
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	// For API key validity
	apiKeyCache debrid.Cache
	// For info_hash instant availability
	availabilityCache debrid.Cache
	cacheAge          time.Duration
	logger            *zap.Logger
}

func NewClient(opts ClientOptions, apiKeyCache, availabilityCache debrid.Cache, logger *zap.Logger) (*Client, error) {
	// Precondition check
	if opts.BaseURL == "" {
		return nil, errors.New("opts.BaseURL must not be empty")
	}

	return &Client{
		baseURL: opts.BaseURL,
		httpClient: &http.Client{
			Timeout: opts.Timeout,
		},
		apiKeyCache:       apiKeyCache,
		availabilityCache: availabilityCache,
		cacheAge:          opts.CacheAge,
		logger:            logger,
	}, nil
}

func (c *Client) TestAPIkey(ctx context.Context, apiKey string) error {
	zapFieldDebridSite := zap.String("AllDebrid", apiKey)
	zapFieldAPIkey := zap.String("apiKey", apiKey)
	c.logger.Debug("Testing API key...", zapFieldDebridSite, zapFieldAPIkey)

	// Check cache first.
	// Note: Only whenan API key is valid a cache item was created, becausean API key is probably valid for another 24 hours, while whenan API key is invalid it's likely that the user makes a payment to AllDebrid to extend his premium status and make his API key valid again *within* 24 hours.
	created, found, err := c.apiKeyCache.Get(apiKey)
	if err != nil {
		c.logger.Error("Couldn't decode API key cache item", zap.Error(err), zapFieldDebridSite, zapFieldAPIkey)
	} else if !found {
		c.logger.Debug("API key not found in cache", zapFieldDebridSite, zapFieldAPIkey)
	} else if time.Since(created) > (24 * time.Hour) {
		expiredSince := time.Since(created.Add(24 * time.Hour))
		c.logger.Debug("API key cached as valid, but item is expired", zap.Duration("expiredSince", expiredSince), zapFieldDebridSite, zapFieldAPIkey)
	} else {
		c.logger.Debug("API key cached as valid", zapFieldDebridSite, zapFieldAPIkey)
		return nil
	}

	resBytes, err := c.get(ctx, c.baseURL+"/v4/user", apiKey)
	if err != nil {
		return fmt.Errorf("Couldn't fetch user info from api.alldebrid.com with the provided API key: %v", err)
	}
	if gjson.GetBytes(resBytes, "status").String() != "success" {
		errMsg := gjson.GetBytes(resBytes, "error.message").String()
		return fmt.Errorf("Got error response from api.alldebrid.com: %v", errMsg)
	}

	c.logger.Debug("API key OK", zapFieldDebridSite, zapFieldAPIkey)

	// Create cache item
	if err = c.apiKeyCache.Set(apiKey); err != nil {
		c.logger.Error("Couldn't cache API key", zap.Error(err), zapFieldDebridSite, zapFieldAPIkey)
	}

	return nil
}

func (c *Client) GetStreamURL(ctx context.Context, magnetURL, apiKey string, remote bool) (string, error) {
	zapFieldDebridSite := zap.String("AllDebrid", apiKey)
	zapFieldAPIkey := zap.String("apiKey", apiKey)
	c.logger.Debug("Adding magnet to AllDebrid...", zapFieldDebridSite, zapFieldAPIkey)
	data := url.Values{}
	data.Set("magnets[]", magnetURL)
	resBytes, err := c.post(ctx, c.baseURL+"/v4/magnet/upload", apiKey, data)
	if err != nil {
		return "", fmt.Errorf("Couldn't add magnet to AllDebrid: %v", err)
	}
	if gjson.GetBytes(resBytes, "status").String() != "success" {
		errMsg := gjson.GetBytes(resBytes, "error.message").String()
		return "", fmt.Errorf("Got error response from api.alldebrid.com: %v", errMsg)
	}
	c.logger.Debug("Finished adding magnet to AllDebrid", zapFieldDebridSite, zapFieldAPIkey)
	ready := gjson.GetBytes(resBytes, "data.magnets.1.ready").Bool()
	if !ready {
		return "", fmt.Errorf("Magnet is not ready")
	}
	adID := gjson.GetBytes(resBytes, "data.magnets.1.id").String()

	// Check AllDebrid magnet status (to get link)

	c.logger.Debug("Checking magnet status...", zapFieldDebridSite, zapFieldAPIkey)
	statusURL := c.baseURL + "/v4/magnet/status?id=" + adID
	resBytes, err = c.get(ctx, statusURL, apiKey)
	if err != nil {
		return "", fmt.Errorf("Couldn't get magnet info from api.alldebrid.com: %v", err)
	}
	if gjson.GetBytes(resBytes, "status").String() != "success" {
		errMsg := gjson.GetBytes(resBytes, "error.message").String()
		return "", fmt.Errorf("Got error response from api.alldebrid.com: %v", errMsg)
	}
	linkResults := gjson.GetBytes(resBytes, "data.magnets.1.links").Array()
	link, err := selectLink(ctx, linkResults)
	if err != nil {
		return "", fmt.Errorf("Couldn't find proper link in magnet status: %v", err)
	}
	c.logger.Debug("Magnet status OK", zapFieldDebridSite, zapFieldAPIkey)

	// Unlock link

	c.logger.Debug("Getting download link...", zapFieldDebridSite, zapFieldAPIkey)
	unlockURL := c.baseURL + "/v4/link/unlock?link=" + link
	resBytes, err = c.get(ctx, unlockURL, apiKey)
	if err != nil {
		return "", fmt.Errorf("Couldn't unrestrict link: %v", err)
	}
	if gjson.GetBytes(resBytes, "status").String() != "success" {
		errMsg := gjson.GetBytes(resBytes, "error.message").String()
		return "", fmt.Errorf("Got error response from api.alldebrid.com: %v", errMsg)
	}
	streamURL := gjson.GetBytes(resBytes, "data.link").String()
	c.logger.Debug("Unlocked link", zap.String("unlockedLink", streamURL), zapFieldDebridSite, zapFieldAPIkey)

	return streamURL, nil
}

func (c *Client) get(ctx context.Context, url, apiKey string) ([]byte, error) {
	if strings.Contains(url, "?") {
		url += "?agent=deflix&apikey=" + apiKey
	} else {
		url += "&agent=deflix&apikey=" + apiKey
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("Couldn't create GET request: %v", err)
	}
	// In case AD blocks requests based on User-Agent
	fakeVersion := strconv.Itoa(rand.Intn(10000))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0."+fakeVersion+".149 Safari/537.36")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Couldn't send GET request: %v", err)
	}
	defer res.Body.Close()

	// Check server response
	if res.StatusCode != http.StatusOK {
		resBody, _ := ioutil.ReadAll(res.Body)
		if len(resBody) == 0 {
			return nil, fmt.Errorf("bad HTTP response status: %v (GET request to '%v')", res.Status, url)
		}
		return nil, fmt.Errorf("bad HTTP response status: %v (GET request to '%v'; response body: '%s')", res.Status, url, resBody)
	}

	return ioutil.ReadAll(res.Body)
}

func (c *Client) post(ctx context.Context, url, apiKey string, data url.Values) ([]byte, error) {
	url += "?agent=deflix&apikey=" + apiKey
	req, err := http.NewRequest("POST", url, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("Couldn't create POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// In case AD blocks requests based on User-Agent
	fakeVersion := strconv.Itoa(rand.Intn(10000))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0."+fakeVersion+".149 Safari/537.36")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Couldn't send POST request: %v", err)
	}
	defer res.Body.Close()

	// Check server response
	if res.StatusCode != http.StatusOK {
		resBody, _ := ioutil.ReadAll(res.Body)
		if len(resBody) == 0 {
			return nil, fmt.Errorf("bad HTTP response status: %v (GET request to '%v')", res.Status, url)
		}
		return nil, fmt.Errorf("bad HTTP response status: %v (GET request to '%v'; response body: '%s')", res.Status, url, resBody)
	}

	return ioutil.ReadAll(res.Body)
}

func selectLink(ctx context.Context, linkResults []gjson.Result) (string, error) {
	// Precondition check
	if len(linkResults) == 0 {
		return "", fmt.Errorf("Empty slice of links")
	}

	var link string
	var size int64
	for _, res := range linkResults {
		if res.Get("size").Int() > size {
			size = res.Get("size").Int()
			link = res.Get("link").String()
		}
	}

	if link == "" {
		return "", fmt.Errorf("No link found")
	}

	return link, nil
}
