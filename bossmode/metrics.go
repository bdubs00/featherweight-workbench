package bossmode

// This file adds the series of the stream to the metrics of the front
// (metrics.RegisterCollector).

import (
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// collector reports the streams of an agent at scrape time.
type collector struct {
	a       *fw.Agent
	open    *prometheus.Desc
	refused *prometheus.Desc
}

// newCollector returns the collector of the stream of a
// (metrics.CollectorCtor). Without a hub, it reports nothing.
func newCollector(a *fw.Agent) prometheus.Collector {
	return &collector{
		a: a,
		open: prometheus.NewDesc("fw_bossmode_streams_open",
			"The streams of the page of bossmode that are open now.", nil, nil),
		refused: prometheus.NewDesc("fw_bossmode_streams_refused_total",
			"The streams of the page of bossmode that the front refused with 429, because MaxStreams were open.", nil, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.open
	ch <- c.refused
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	h := Of(c.a)
	if h == nil {
		return
	}
	h.mu.Lock()
	open, refused := h.streams, h.refused
	h.mu.Unlock()
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(open))
	ch <- prometheus.MustNewConstMetric(c.refused, prometheus.CounterValue, float64(refused))
}
