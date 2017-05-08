package exporter

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/garyburd/redigo/redis"
	"github.com/prometheus/client_golang/prometheus"
)

// RedisHost represents a set of Redis Hosts to health check.
type RedisHost struct {
	Addrs     []string
	Passwords []string
	Aliases   []string
}

type dbKeyPair struct {
	db, key string
}

// Exporter implements the prometheus.Exporter interface, and exports Redis metrics.
type Exporter struct {
	redis        RedisHost
	namespace    string
	keys         []dbKeyPair
	keyValues    *prometheus.GaugeVec
	keySizes     *prometheus.GaugeVec
	duration     prometheus.Gauge
	scrapeErrors prometheus.Gauge
	totalScrapes prometheus.Counter
	metrics      map[string]*prometheus.GaugeVec
	metricsMtx   sync.RWMutex
	sync.RWMutex
}

type scrapeResult struct {
	Name  string
	Value float64
	Addr  string
	Alias string
	DB    string
}

var (
	metricMap = map[string]string{
		// # Server
		"uptime_in_seconds": "uptime_in_seconds",

		// # Clients
		"connected_clients": "connected_clients",
		"blocked_clients":   "blocked_clients",

		// # Memory
		"used_memory":             "memory_used_bytes",
		"used_memory_rss":         "memory_used_rss_bytes",
		"used_memory_peak":        "memory_used_peak_bytes",
		"used_memory_lua":         "memory_used_lua_bytes",
		"maxmemory":               "memory_max_bytes",
		"mem_fragmentation_ratio": "memory_fragmentation_ratio",

		// # Persistence
		"rdb_changes_since_last_save":  "rdb_changes_since_last_save",
		"rdb_last_bgsave_time_sec":     "rdb_last_bgsave_duration_sec",
		"rdb_current_bgsave_time_sec":  "rdb_current_bgsave_duration_sec",
		"aof_enabled":                  "aof_enabled",
		"aof_rewrite_in_progress":      "aof_rewrite_in_progress",
		"aof_rewrite_scheduled":        "aof_rewrite_scheduled",
		"aof_last_rewrite_time_sec":    "aof_last_rewrite_duration_sec",
		"aof_current_rewrite_time_sec": "aof_current_rewrite_duration_sec",

		// # Stats
		"total_connections_received": "connections_received_total",
		"total_commands_processed":   "commands_processed_total",
		"total_net_input_bytes":      "net_input_bytes_total",
		"total_net_output_bytes":     "net_output_bytes_total",
		"rejected_connections":       "rejected_connections_total",
		"expired_keys":               "expired_keys_total",
		"evicted_keys":               "evicted_keys_total",
		"keyspace_hits":              "keyspace_hits_total",
		"keyspace_misses":            "keyspace_misses_total",
		"pubsub_channels":            "pubsub_channels",
		"pubsub_patterns":            "pubsub_patterns",
    "instantaneous_ops_per_sec":  "instantaneous_ops_per_sec",

		// # Replication
		"loading":                    "loading_dump_file",
		"connected_slaves":           "connected_slaves",
		"repl_backlog_size":          "replication_backlog_bytes",
    "master_last_io_seconds_ago": "master_last_io_seconds_ago",
    "master_repl_offset":         "master_repl_offset",

		// # CPU
		"used_cpu_sys":           "used_cpu_sys",
		"used_cpu_user":          "used_cpu_user",
		"used_cpu_sys_children":  "used_cpu_sys_children",
		"used_cpu_user_children": "used_cpu_user_children",

		// # Cluster
		"cluster_stats_messages_sent":     "cluster_messages_sent_total",
		"cluster_stats_messages_received": "cluster_messages_received_total",
	}
)

func (e *Exporter) initGauges() {

	e.metrics = map[string]*prometheus.GaugeVec{}
	e.metrics["db_keys"] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: e.namespace,
		Name:      "db_keys",
		Help:      "Total number of keys by DB",
	}, []string{"addr", "alias", "db"})
	e.metrics["db_keys_expiring"] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: e.namespace,
		Name:      "db_keys_expiring",
		Help:      "Total number of expiring keys by DB",
	}, []string{"addr", "alias", "db"})
	e.metrics["db_avg_ttl_seconds"] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: e.namespace,
		Name:      "db_avg_ttl_seconds",
		Help:      "Avg TTL in seconds",
	}, []string{"addr", "alias", "db"})

	// Emulate a Summary.
	e.metrics["command_call_duration_seconds_count"] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: e.namespace,
		Name:      "command_call_duration_seconds_count",
		Help:      "Total number of calls per command",
	}, []string{"addr", "alias", "cmd"})
	e.metrics["command_call_duration_seconds_sum"] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: e.namespace,
		Name:      "command_call_duration_seconds_sum",
		Help:      "Total amount of time in seconds spent per command",
	}, []string{"addr", "alias", "cmd"})
}

// NewRedisExporter returns a new exporter of Redis metrics.
// note to self: next time we add an argument, instead add a RedisExporter struct
func NewRedisExporter(host RedisHost, namespace, checkKeys string) (*Exporter, error) {

	e := Exporter{
		redis:     host,
		namespace: namespace,
		keyValues: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "key_value",
			Help:      "The value of \"key\"",
		}, []string{"addr", "alias", "db", "key"}),
		keySizes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "key_size",
			Help:      "The length or size of \"key\"",
		}, []string{"addr", "alias", "db", "key"}),
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "exporter_last_scrape_duration_seconds",
			Help:      "The last scrape duration.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_scrapes_total",
			Help:      "Current total redis scrapes.",
		}),
		scrapeErrors: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "exporter_last_scrape_error",
			Help:      "The last scrape error status.",
		}),
	}
	for _, k := range strings.Split(checkKeys, ",") {
		var err error
		db := "0"
		key := ""
		frags := strings.Split(k, "=")
		switch len(frags) {
		case 1:
			db = "0"
			key, err = url.QueryUnescape(strings.TrimSpace(frags[0]))
		case 2:
			db = strings.Replace(strings.TrimSpace(frags[0]), "db", "", -1)
			key, err = url.QueryUnescape(strings.TrimSpace(frags[1]))
		default:
			err = fmt.Errorf("")
		}
		if err != nil {
			log.Debugf("Couldn't parse db/key string: %s", k)
			continue
		}
		if key != "" {
			e.keys = append(e.keys, dbKeyPair{db, key})
		}
	}

	e.initGauges()
	return &e, nil
}

// Describe outputs Redis metric descriptions.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {

	for _, m := range e.metrics {
		m.Describe(ch)
	}
	e.keySizes.Describe(ch)
	e.keyValues.Describe(ch)

	ch <- e.duration.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.scrapeErrors.Desc()
}

// Collect fetches new metrics from the RedisHost and updates the appropriate metrics.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	scrapes := make(chan scrapeResult)

	e.Lock()
	defer e.Unlock()

	e.initGauges()
	go e.scrape(scrapes)
	e.setMetrics(scrapes)

	e.keySizes.Collect(ch)
	e.keyValues.Collect(ch)

	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.scrapeErrors
	e.collectMetrics(ch)
}

func includeMetric(s string) bool {

	if strings.HasPrefix(s, "db") || strings.HasPrefix(s, "cmdstat_") || strings.HasPrefix(s, "cluster_") {
		return true
	}

	_, ok := metricMap[s]

	return ok
}

func extractVal(s string) (val float64, err error) {
	split := strings.Split(s, "=")
	if len(split) != 2 {
		return 0, fmt.Errorf("nope")
	}
	val, err = strconv.ParseFloat(split[1], 64)
	if err != nil {
		return 0, fmt.Errorf("nope")
	}
	return
}

/*
	valid example: db0:keys=1,expires=0,avg_ttl=0
*/
func parseDBKeyspaceString(db string, stats string) (keysTotal float64, keysExpiringTotal float64, avgTTL float64, ok bool) {
	ok = false
	if !strings.HasPrefix(db, "db") {
		return
	}

	split := strings.Split(stats, ",")
	if len(split) != 3 && len(split) != 2 {
		return
	}

	var err error
	ok = true
	if keysTotal, err = extractVal(split[0]); err != nil {
		ok = false
		return
	}
	if keysExpiringTotal, err = extractVal(split[1]); err != nil {
		ok = false
		return
	}

	avgTTL = -1
	if len(split) > 2 {
		if avgTTL, err = extractVal(split[2]); err != nil {
			ok = false
			return
		}
		avgTTL /= 1000
	}
	return
}

func (e *Exporter) extractInfoMetrics(info, addr string, alias string, scrapes chan<- scrapeResult) error {
	cmdstats := false
	lines := strings.Split(info, "\r\n")
	for _, line := range lines {
		log.Debugf("info: %s", line)
		if len(line) > 0 && line[0] == '#' {
			if strings.Contains(line, "Commandstats") {
				cmdstats = true
			}
			continue
		}

		if (len(line) < 2) || (!strings.Contains(line, ":")) {
			cmdstats = false
			continue
		}

		split := strings.Split(line, ":")
		if len(split) != 2 || !includeMetric(split[0]) {
			continue
		}

		if cmdstats {
			/*
				cmdstat_get:calls=21,usec=175,usec_per_call=8.33
				cmdstat_set:calls=61,usec=3139,usec_per_call=51.46
				cmdstat_setex:calls=75,usec=1260,usec_per_call=16.80
			*/
			frags := strings.Split(split[0], "_")
			if len(frags) != 2 {
				continue
			}

			cmd := frags[1]

			frags = strings.Split(split[1], ",")
			if len(frags) != 3 {
				continue
			}

			var calls float64
			var usecTotal float64
			var err error
			if calls, err = extractVal(frags[0]); err != nil {
				continue
			}
			if usecTotal, err = extractVal(frags[1]); err != nil {
				continue
			}

			e.metricsMtx.RLock()
			e.metrics["command_call_duration_seconds_count"].WithLabelValues(addr, alias, cmd).Set(calls)
			e.metrics["command_call_duration_seconds_sum"].WithLabelValues(addr, alias, cmd).Set(usecTotal / 1e6)
			e.metricsMtx.RUnlock()
			continue
		}

		if keysTotal, keysEx, avgTTL, ok := parseDBKeyspaceString(split[0], split[1]); ok {
			scrapes <- scrapeResult{Name: "db_keys", Addr: addr, Alias: alias, DB: split[0], Value: keysTotal}
			scrapes <- scrapeResult{Name: "db_keys_expiring", Addr: addr, Alias: alias, DB: split[0], Value: keysEx}
			if avgTTL > -1 {
				scrapes <- scrapeResult{Name: "db_avg_ttl_seconds", Addr: addr, Alias: alias, DB: split[0], Value: avgTTL}
			}
			continue
		}

		metricName := split[0]
		if newName, ok := metricMap[metricName]; ok {
			metricName = newName
		}

		var err error
		var val float64

		switch split[1] {

		case "ok":
			val = 1

		case "fail":
			val = 0

		default:
			val, err = strconv.ParseFloat(split[1], 64)

		}
		if err != nil {
			log.Debugf("couldn't parse %s, err: %s", split[1], err)
			continue
		}

		scrapes <- scrapeResult{Name: metricName, Addr: addr, Alias: alias, Value: val}
	}
	return nil
}

func (e *Exporter) scrape(scrapes chan<- scrapeResult) {

	defer close(scrapes)
	now := time.Now().UnixNano()
	e.totalScrapes.Inc()

	errorCount := 0
	for idx, addr := range e.redis.Addrs {
		var c redis.Conn
		var err error
		alias := e.redis.Aliases[idx]

		scrapes <- scrapeResult{Name: "up", Addr: addr, Alias: alias, Value: 0}

		var options []redis.DialOption
		if len(e.redis.Passwords) > idx && e.redis.Passwords[idx] != "" {
			options = append(options, redis.DialPassword(e.redis.Passwords[idx]))
		}

		log.Debugf("Trying DialURL(): %s", addr)
		if c, err = redis.DialURL(addr, options...); err != nil {
			log.Debugf("DialURL() failed, err: %s", err)
			frags := strings.Split(addr, "://")
			if len(frags) == 2 {
				log.Debugf("Trying: Dial(): %s %s", frags[0], frags[1])
				c, err = redis.Dial(frags[0], frags[1], options...)
			} else {
				log.Debugf("Trying: Dial(): tcp %s", addr)
				c, err = redis.Dial("tcp", addr, options...)
			}
		}

		if err != nil {
			log.Printf("redis err: %s", err)
			errorCount++
			continue
		}
		defer c.Close()
		log.Debugf("connected to: %s", addr)

		info, err := redis.String(c.Do("INFO", "ALL"))
		if err == nil {
			err = e.extractInfoMetrics(info, addr, alias, scrapes)
		} else {
			log.Printf("redis err: %s", err)
			errorCount++
			continue
		}

		if strings.Index(info, "cluster_enabled:1") != -1 {
			info, err = redis.String(c.Do("CLUSTER", "INFO"))
			if err == nil {
				err = e.extractInfoMetrics(info, addr, alias, scrapes)
			} else {
				log.Printf("redis err: %s", err)
				errorCount++
				continue
			}
		}

		scrapes <- scrapeResult{Name: "up", Addr: addr, Alias: alias, Value: 1}

		for _, k := range e.keys {
			if _, err := c.Do("SELECT", k.db); err != nil {
				continue
			}
			if tempVal, err := c.Do("GET", k.key); err == nil && tempVal != nil {
				if val, err := strconv.ParseFloat(fmt.Sprintf("%s", tempVal), 64); err == nil {
					e.keyValues.WithLabelValues(addr, alias, "db"+k.db, k.key).Set(val)
				}
			}

			for _, op := range []string{
				"HLEN",
				"LLEN",
				"SCARD",
				"ZCARD",
				"PFCOUNT",
				"STRLEN",
			} {
				if tempVal, err := c.Do(op, k.key); err == nil && tempVal != nil {
					e.keySizes.WithLabelValues(addr, alias, "db"+k.db, k.key).Set(float64(tempVal.(int64)))
					break
				}
			}
		}
	}

	e.scrapeErrors.Set(float64(errorCount))
	e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
}

func (e *Exporter) setMetrics(scrapes <-chan scrapeResult) {
	for scr := range scrapes {
		name := scr.Name
		if _, ok := e.metrics[name]; !ok {
			e.metricsMtx.Lock()
			e.metrics[name] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Namespace: e.namespace,
				Name:      name,
			}, []string{"addr", "alias"})
			e.metricsMtx.Unlock()
		}
		var labels prometheus.Labels = map[string]string{"addr": scr.Addr, "alias": scr.Alias}
		if len(scr.DB) > 0 {
			labels["db"] = scr.DB
		}
		e.metrics[name].With(labels).Set(float64(scr.Value))
	}
}

func (e *Exporter) collectMetrics(metrics chan<- prometheus.Metric) {
	for _, m := range e.metrics {
		m.Collect(metrics)
	}
}
