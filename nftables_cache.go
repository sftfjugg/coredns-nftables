package coredns_nftables

import (
	"container/list"
	"sync"
	"time"

	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/google/nftables"
	lru "github.com/hashicorp/golang-lru"
	"github.com/miekg/dns"
	"github.com/vishvananda/netns"
)

var log = clog.NewWithPlugin("nftables")
var cacheLock sync.Mutex = sync.Mutex{}
var cacheList = list.New()
var cacheExpiredDuration time.Duration = time.Minute * time.Duration(5)
var setLruMaxRetryTimes int = 2147483647
var setLruMaxCount int = 10000
var setLruTimeout time.Duration = time.Hour * time.Duration(720)

type NftableCache struct {
	table    *nftables.Table
	setCache map[string]*map[string]time.Time
}

type NftableIPCache struct {
	ExpireTime time.Time
	ApplyCount int
}

type NftablesCache struct {
	tables                    map[nftables.TableFamily]*map[string]*NftableCache
	recentlyIPCache           *lru.Cache
	CreateTimepoint           time.Time
	NftableConnection         *nftables.Conn
	NetworkNamespace          netns.NsHandle
	HasNftableConnectionError bool
}

func NewCache() (*NftablesCache, error) {
	{
		cacheLock.Lock()
		defer cacheLock.Unlock()

		// Destroy timeout connections
		for cacheList.Front() != nil {
			cacheHead := cacheList.Front().Value.(*NftablesCache)
			cacheList.Remove(cacheList.Front())

			if time.Since(cacheHead.CreateTimepoint) > cacheExpiredDuration {
				go cacheHead.destroy()
			} else {
				log.Debugf("Nftables connection select %p from pool", cacheHead)
				cacheHead.gc()
				return cacheHead, nil
			}
		}
	}

	c, newNS, err := openSystemNFTConn()
	if err != nil {
		return nil, err
	}

	lruCache, _ := lru.New(setLruMaxCount)
	ret := &NftablesCache{
		tables:                    make(map[nftables.TableFamily]*map[string]*NftableCache),
		recentlyIPCache:           lruCache,
		CreateTimepoint:           time.Now(),
		NftableConnection:         c,
		NetworkNamespace:          newNS,
		HasNftableConnectionError: false,
	}

	log.Infof("Nftables create new cache pool %p", ret)
	return ret, nil
}

func (cache *NftablesCache) LruIgnoreIp(answer *dns.RR) bool {
	if cache.recentlyIPCache == nil {
		return false
	}

	var ip string = ""
	switch (*answer).Header().Rrtype {
	case dns.TypeA:
		ip = (*answer).(*dns.A).A.String()
	case dns.TypeAAAA:
		ip = (*answer).(*dns.AAAA).AAAA.String()
	default:
		return false
	}

	if len(ip) == 0 {
		return false
	}

	value, ok := cache.recentlyIPCache.Get(ip)
	if ok {
		return value.(*NftableIPCache).ApplyCount >= setLruMaxRetryTimes
	}

	return false
}

func (cache *NftablesCache) LruUpdateIp(answer *dns.RR, rulesCounter int) {
	if cache.recentlyIPCache == nil {
		return
	}

	var ip string = ""
	switch (*answer).Header().Rrtype {
	case dns.TypeA:
		ip = (*answer).(*dns.A).A.String()
	case dns.TypeAAAA:
		ip = (*answer).(*dns.AAAA).AAAA.String()
	default:
		return
	}

	if len(ip) == 0 {
		return
	}

	log.Infof("Nftables apply %v rule(s) for %v(%v) done", rulesCounter, ip, (*answer).Header().Name)
	value, ok := cache.recentlyIPCache.Get(ip)
	if ok {
		value.(*NftableIPCache).ApplyCount += 1
	} else {
		cache.recentlyIPCache.Add(ip, &NftableIPCache{
			ExpireTime: time.Now().Add(setLruTimeout),
			ApplyCount: 1,
		})
	}
}

func (cache *NftablesCache) gc() {
	if cache.recentlyIPCache == nil {
		return
	}

	now := time.Now()
	for cache.recentlyIPCache.Len() != 0 {
		_, value, ok := cache.recentlyIPCache.GetOldest()
		if !ok {
			break
		}

		if value.(*NftableIPCache).ExpireTime.After(now) {
			break
		}

		cache.recentlyIPCache.RemoveOldest()
	}
}

func (cache *NftablesCache) destroy() error {
	log.Infof("Nftables cache pool %p start to destroy", cache)

	cleanupSystemNFTConn(cache.NetworkNamespace)
	return nil
}

func CloseCache(cache *NftablesCache) error {
	err := cache.NftableConnection.Flush()
	if err != nil {
		log.Errorf("Nftables Flush connection failed %v", err)
		cache.HasNftableConnectionError = true
	}

	if cache.HasNftableConnectionError || time.Since(cache.CreateTimepoint) > cacheExpiredDuration {
		return cache.destroy()
	}

	cacheLock.Lock()
	defer cacheLock.Unlock()

	cacheList.PushBack(cache)
	log.Debugf("Nftables connection %p add to cache pool", cache)

	return nil
}

func ClearCache() {
	cacheLock.Lock()
	defer cacheLock.Unlock()

	// Destroy timeout connections
	for cacheList.Front() != nil {
		cacheHead := cacheList.Front().Value.(*NftablesCache)
		cacheList.Remove(cacheList.Front())

		go cacheHead.destroy()
	}
}

func (cache *NftablesCache) MutableNftablesTable(family nftables.TableFamily, tableName string) *NftableCache {
	tableSet, ok := (*cache).tables[family]
	if !ok {
		tableSetM := make(map[string]*NftableCache)
		tableSet = &tableSetM
		(*cache).tables[family] = tableSet
	}

	if len(*tableSet) == 0 {
		familName := (*cache).GetFamilyName(family)
		tables, _ := cache.NftableConnection.ListTablesOfFamily(family)
		if tables != nil {
			log.Debugf("Nftables %v table(s) of %v found", len(tables), familName)
			for _, table := range tables {
				log.Debugf("\t - %v", table.Name)
				(*tableSet)[(*table).Name] = &NftableCache{
					table: table,
				}
			}
		}
	}

	tableCache, ok := (*tableSet)[tableName]
	if !ok {
		tableCache = &NftableCache{
			table: &nftables.Table{
				Family: family,
				Name:   tableName,
			},
		}
		log.Debugf("Nftables try to create table %v %v", (*cache).GetFamilyName(family), tableName)
		(*tableSet)[tableName] = tableCache
		tableCache.table = cache.NftableConnection.AddTable(tableCache.table)
	}

	return tableCache
}

func (cache *NftablesCache) SetAddElements(tableCache *NftableCache, set *nftables.Set, elements []nftables.SetElement) error {
	err := cache.NftableConnection.SetAddElements(set, elements)
	if err != nil {
		cache.HasNftableConnectionError = true
	}

	return err
}

func (cache *NftablesCache) GetFamilyName(family nftables.TableFamily) string {
	switch family {
	case nftables.TableFamilyUnspecified:
		return "unspecified"
	case nftables.TableFamilyINet:
		return "inet"
	case nftables.TableFamilyIPv4:
		return "ipv4"
	case nftables.TableFamilyIPv6:
		return "ipv6"
	case nftables.TableFamilyARP:
		return "arp"
	case nftables.TableFamilyNetdev:
		return "netdev"
	case nftables.TableFamilyBridge:
		return "bridge"
	}

	return "unknown"
}

// openSystemNFTConn returns a netlink connection that tests against
// the running kernel in a separate network namespace.
// cleanupSystemNFTConn() must be called from a defer to cleanup
// created network namespace.
func openSystemNFTConn() (*nftables.Conn, netns.NsHandle, error) {
	// We lock the goroutine into the current thread, as namespace operations
	// such as those invoked by `netns.New()` are thread-local. This is undone
	// in cleanupSystemNFTConn().
	// runtime.LockOSThread()

	// ns, err := netns.New()
	// if err != nil {
	// 	log.Errorf("netns.New() failed: %v", err)
	// 	return nil, 0, err
	// }
	// c, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	c, err := nftables.New()
	if err != nil {
		log.Errorf("Nftables call nftables.New() failed: %v", err)
	}
	// return c, ns, err
	return c, 0, err
}

func cleanupSystemNFTConn(newNS netns.NsHandle) {
	// defer runtime.UnlockOSThread()

	if newNS == 0 {
		return
	}
	if err := newNS.Close(); err != nil {
		log.Errorf("Nftables call newNS.Close() failed: %v", err)
	}
}

func SetConnectionTimeout(timeout time.Duration) {
	cacheExpiredDuration = timeout
}

func SetSetLruTimeout(timeout time.Duration) {
	setLruTimeout = timeout
}

func SetSetLruMaxCount(count int) {
	setLruMaxCount = count
}

func SetSetLruMaxRetryTimes(times int) {
	setLruMaxRetryTimes = times
}
