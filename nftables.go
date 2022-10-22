package coredns_nftables

import (
	"context"
	"fmt"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	"github.com/coredns/coredns/plugin/pkg/nonwriter"
	"github.com/miekg/dns"

	"github.com/google/nftables"
)

var asyncMode bool = false

type NftablesRuleSet struct {
	RuleAddElement []*NftablesSetAddElement
}

// NftablesHandler implements the plugin.Handler interface.
type NftablesHandler struct {
	Next plugin.Handler

	Rules map[nftables.TableFamily]*NftablesRuleSet
}

func NewNftablesHandler() NftablesHandler {
	return NftablesHandler{
		Next:  nil,
		Rules: make(map[nftables.TableFamily]*NftablesRuleSet),
	}
}

func (m *NftablesHandler) ServeWorker(ctx context.Context, r *dns.Msg) error {
	cache, err := NewCache()
	if err != nil {
		log.Errorf("NewCache failed, %v", err)
		return err
	}
	defer CloseCache(cache)
	defer exportRecordDuration(ctx, time.Now())

	for _, answer := range r.Answer {
		var tableFamilies []nftables.TableFamily = nil

		switch answer.Header().Rrtype {
		case dns.TypeA:
			{
				if cache.LruIgnoreIp(&answer) {
					log.Debugf("Ignore ip element %v(%v) because lru max retry times exceeded", answer.(*dns.A).A.String(), answer.Header().Name)
				} else {
					recordCount.WithLabelValues(metrics.WithServer(ctx)).Inc()
					tableFamilies = []nftables.TableFamily{nftables.TableFamilyIPv4, nftables.TableFamilyINet, nftables.TableFamilyBridge}
				}
			}
		case dns.TypeAAAA:
			{
				if cache.LruIgnoreIp(&answer) {
					log.Debugf("Ignore ip element %v(%v) because lru max retry times exceeded", answer.(*dns.AAAA).AAAA.String(), answer.Header().Name)
				} else {
					recordCount.WithLabelValues(metrics.WithServer(ctx)).Inc()
					tableFamilies = []nftables.TableFamily{nftables.TableFamilyIPv6, nftables.TableFamilyINet, nftables.TableFamilyBridge}
				}
			}
		default:
			{
				// do nohting
			}
		}

		if tableFamilies == nil {
			continue
		}

		hasError := false
		applyCounter := 0
		for _, family := range tableFamilies {
			ruleSet, ok := m.Rules[family]
			if ok {
				for _, rule := range ruleSet.RuleAddElement {
					err, ignored := rule.ServeDNS(ctx, cache, &answer, family)
					if err != nil {
						hasError = true
						switch answer.Header().Rrtype {
						case dns.TypeA:
							log.Errorf("Add element %v(%v) to %v %v %v failed.%v", answer.(*dns.A).A.String(), answer.Header().Name, cache.GetFamilyName(family), rule.TableName, rule.SetName, err)
						case dns.TypeAAAA:
							log.Errorf("Add element %v(%v) to %v %v %v failed.%v", answer.(*dns.AAAA).AAAA.String(), answer.Header().Name, cache.GetFamilyName(family), rule.TableName, rule.SetName, err)
						default:
							log.Errorf("Add element %v(%v) to %v %v %v failed.%v", answer.String(), answer.Header().Name, cache.GetFamilyName(family), rule.TableName, rule.SetName, err)
						}
					} else if !ignored {
						applyCounter += 1
					}
				}
			}
		}

		if !hasError {
			cache.LruUpdateIp(&answer, applyCounter)
		}
	}

	return err
}

func (m *NftablesHandler) Name() string { return "nftables" }

func (m *NftablesHandler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	startTime := time.Now()
	nw := nonwriter.New(w)
	rcode, err := plugin.NextOrFailure(m.Name(), m.Next, ctx, nw, r)
	if err != nil {
		return rcode, err
	}
	nextPluginTime := time.Now()

	r = nw.Msg
	if r == nil {
		return dns.RcodeFormatError, fmt.Errorf("no answer received")
	}

	var hasValidRecord bool = false
	for _, answer := range r.Answer {
		if answer.Header().Rrtype == dns.TypeA || answer.Header().Rrtype == dns.TypeAAAA {
			hasValidRecord = true
			break
		}
	}
	if !hasValidRecord {
		log.Debug("Request didn't contain any answer or A/AAAA record")
		err = w.WriteMsg(r)
		if err != nil {
			return dns.RcodeFormatError, err
		}

		return dns.RcodeSuccess, nil
	}

	if asyncMode {
		copyMsg := r.Copy()
		err = w.WriteMsg(r)

		go m.ServeWorker(context.Background(), copyMsg)
		if err != nil {
			return dns.RcodeServerFailure, err
		}
	} else {
		m.ServeWorker(context.Background(), r)
		err = w.WriteMsg(r)
	}

	endTime := time.Now()
	log.Infof("Process %v DNS answers for %v, next plugin cost %vus self cost %vus",
		nextPluginTime.Sub(startTime).Microseconds(), endTime.Sub(nextPluginTime).Microseconds())

	return rcode, nil
}

func (m *NftablesHandler) MutableRuleSet(family nftables.TableFamily) *NftablesRuleSet {
	ret, ok := m.Rules[family]
	if ok {
		return ret
	} else {
		ret = &NftablesRuleSet{}
		m.Rules[family] = ret
		return ret
	}
}

func exportRecordDuration(ctx context.Context, start time.Time) {
	recordDuration.WithLabelValues(metrics.WithServer(ctx)).
		Observe(float64(time.Since(start).Microseconds()))
}

func SetNftableAsyncMode(mode bool) {
	asyncMode = mode
}
