package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	gocache "github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"

	"github.com/doingodswork/deflix-stremio/pkg/cinemata"
	"github.com/doingodswork/deflix-stremio/pkg/imdb2torrent"
	"github.com/doingodswork/deflix-stremio/pkg/realdebrid"
)

func registerTypes() {
	// For RealDebrid availability and token cache
	gob.Register(time.Time{})
	// For cinemata cache
	gob.Register(cinemata.CacheItem{})
	// For redirect cache
	gob.Register([]imdb2torrent.Result{})
	// For stream cache
	gob.Register(cacheItem{})
}

type cacheItem struct {
	Value   string
	Created time.Time
}

var _ imdb2torrent.Cache = (*resultCache)(nil)

// resultCache is the cache for imdb2torrent.Result objects, backed by github.com/VictoriaMetrics/fastcache.
type resultCache struct {
	cache *fastcache.Cache
}

// Set implements the imdb2torrent.Cache interface.
func (c resultCache) Set(key string, results []imdb2torrent.Result) error {
	item := imdb2torrent.CacheItem{
		Results: results,
		Created: time.Now(),
	}
	return gobSet(c.cache, key, item)
}

// Get implements the imdb2torrent.Cache interface.
func (c resultCache) Get(key string) ([]imdb2torrent.Result, time.Time, bool, error) {
	var item imdb2torrent.CacheItem
	found, err := gobGet(c.cache, key, &item)
	return item.Results, item.Created, found, err
}

var _ cinemata.Cache = (*movieCache)(nil)

// movieCache is the cache for cinemata.Movie objects.
type movieCache struct {
	cache *gocache.Cache
}

// Set implements the cinemata.Cache interface.
func (c movieCache) Set(key string, movie cinemata.Movie) error {
	item := cinemata.CacheItem{
		Movie:   movie,
		Created: time.Now(),
	}
	c.cache.Set(key, item, 0)
	return nil
}

// Get implements the cinemata.Cache interface.
func (c movieCache) Get(key string) (cinemata.Movie, time.Time, bool, error) {
	itemIface, found := c.cache.Get(key)
	if !found {
		return cinemata.Movie{}, time.Time{}, found, nil
	}
	item, ok := itemIface.(cinemata.CacheItem)
	if !ok {
		return cinemata.Movie{}, time.Time{}, found, fmt.Errorf("Couldn't cast cached value to cinemata.CacheItem: type was: %T", itemIface)
	}
	return item.Movie, item.Created, found, nil
}

var _ realdebrid.Cache = (*creationCache)(nil)

// creationCache caches if a key exists and the time this was cached.
type creationCache struct {
	cache *gocache.Cache
}

// Set implements the cinemata.Cache interface.
func (c creationCache) Set(key string) error {
	c.cache.Set(key, time.Now(), 0)
	return nil
}

// Get implements the cinemata.Cache interface.
func (c creationCache) Get(key string) (time.Time, bool, error) {
	createdIface, found := c.cache.Get(key)
	if !found {
		return time.Time{}, found, nil
	}
	created, ok := createdIface.(time.Time)
	if !ok {
		return time.Time{}, found, fmt.Errorf("Couldn't cast cached value to time.Time: type was: %T", createdIface)
	}
	return created, found, nil
}

func gobSet(cache *fastcache.Cache, key string, item interface{}) error {
	writer := bytes.Buffer{}
	encoder := gob.NewEncoder(&writer)
	if err := encoder.Encode(item); err != nil {
		return fmt.Errorf("Couldn't encode item: %v", err)
	}
	cache.Set([]byte(key), writer.Bytes())
	return nil
}

func gobGet(cache *fastcache.Cache, key string, item interface{}) (bool, error) {
	data, found := cache.HasGet(nil, []byte(key))
	if !found {
		return found, nil
	}
	reader := bytes.NewReader(data)
	decoder := gob.NewDecoder(reader)
	if err := decoder.Decode(item); err != nil {
		return found, fmt.Errorf("Couldn't decode item: %v", err)
	}
	return found, nil
}

func saveGoCache(items map[string]gocache.Item, filePath string) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("Couldn't create go-cache file: %v", err)
	}
	encoder := gob.NewEncoder(file)
	if err = encoder.Encode(items); err != nil {
		return fmt.Errorf("Couldn't encode items for go-cache file: %v", err)
	}
	return nil
}

func loadGoCache(filePath string) (map[string]gocache.Item, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("Couldn't open go-cache file: %v", err)
	}
	decoder := gob.NewDecoder(file)
	result := map[string]gocache.Item{}
	if err = decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("Couldn't decode items from go-cache file: %v", err)
	}
	return result, nil
}

func persistCaches(ctx context.Context, cacheFilePath string, stoppingPtr *bool, fastCaches map[string]*fastcache.Cache, goCaches map[string]*gocache.Cache) {
	// TODO: We might want to overthink this - persisting caches on shutdown might be useful, especially for the redirect cache!
	if *stoppingPtr {
		log.Warn("Regular cache persistence triggered, but server is shutting down")
		return
	}

	log.WithField("cacheFilePath", cacheFilePath).Info("Persisting caches...")
	start := time.Now()

	for name, fastCache := range fastCaches {
		if err := fastCache.SaveToFileConcurrent(cacheFilePath+"/"+name, runtime.NumCPU()); err != nil {
			log.WithError(err).WithField("cache", name).Error("Couldn't save cache to file")
		}
	}

	for name, goCache := range goCaches {
		if err := saveGoCache(goCache.Items(), cacheFilePath+"/"+name+".gob"); err != nil {
			log.WithError(err).WithField("cache", name).Error("Couldn't save cache to file")
		}
	}

	duration := time.Since(start).Milliseconds()
	durationString := strconv.FormatInt(duration, 10) + "ms"
	log.WithField("duration", durationString).Info("Persisted caches")
}

func logCacheStats(ctx context.Context, fastCaches map[string]*fastcache.Cache, goCaches map[string]*gocache.Cache) {
	stats := fastcache.Stats{}
	for name, fastCache := range fastCaches {
		fastCache.UpdateStats(&stats)
		fields := log.Fields{
			"cache":        name,
			"GetCalls":     stats.GetCalls,
			"SetCalls":     stats.SetCalls,
			"Misses":       stats.Misses,
			"Collisions":   stats.Collisions,
			"Corruptions":  stats.Corruptions,
			"EntriesCount": stats.EntriesCount,
			"Size":         strconv.FormatUint(stats.BytesSize/uint64(1024)/uint64(1024), 10) + "MB",
		}
		log.WithFields(fields).Info("Cache stats")
		stats.Reset()
	}

	for name, goCache := range goCaches {
		log.WithField("cache", name).WithField("itemCount", goCache.ItemCount()).Info("Cache stats")
	}
}
