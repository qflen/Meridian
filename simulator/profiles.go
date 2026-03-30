// Package simulator generates realistic infrastructure metrics for Meridian demos.
package simulator

// HostProfile defines a simulated host with its role and associated metrics.
type HostProfile struct {
	Hostname string
	Role     string
	Metrics  []MetricProfile
}

// MetricProfile defines a single metric's generation behavior.
type MetricProfile struct {
	Name     string
	Type     MetricType
	BaseVal  float64
	NoiseAmp float64
	Labels   map[string]string
}

// MetricType determines the value generation strategy.
type MetricType int

const (
	// MetricGauge fluctuates around a base value with noise.
	MetricGauge MetricType = iota
	// MetricCounter monotonically increases.
	MetricCounter
	// MetricGaugeCPU follows a diurnal pattern with spikes.
	MetricGaugeCPU
	// MetricGaugeMemory shows slow upward drift with periodic drops.
	MetricGaugeMemory
	// MetricGaugeRatio stays between 0 and 1.
	MetricGaugeRatio
)

// DefaultProfiles returns the standard 8-host infrastructure setup.
func DefaultProfiles() []HostProfile {
	return []HostProfile{
		webProfile("web-01"),
		webProfile("web-02"),
		webProfile("web-03"),
		dbProfile("db-01"),
		dbProfile("db-02"),
		cacheProfile("cache-01"),
		queueProfile("queue-01"),
		lbProfile("lb-01"),
	}
}

func webProfile(host string) HostProfile {
	labels := map[string]string{"host": host, "role": "web"}
	return HostProfile{
		Hostname: host,
		Role:     "web",
		Metrics: []MetricProfile{
			{Name: "http_requests_total", Type: MetricCounter, BaseVal: 100, NoiseAmp: 20, Labels: labels},
			{Name: "http_request_duration_seconds", Type: MetricGauge, BaseVal: 0.05, NoiseAmp: 0.02, Labels: labels},
			{Name: "http_active_connections", Type: MetricGauge, BaseVal: 120, NoiseAmp: 30, Labels: labels},
			{Name: "cpu_usage_percent", Type: MetricGaugeCPU, BaseVal: 45, NoiseAmp: 5, Labels: labels},
			{Name: "memory_usage_bytes", Type: MetricGaugeMemory, BaseVal: 4e9, NoiseAmp: 1e8, Labels: labels},
		},
	}
}

func dbProfile(host string) HostProfile {
	labels := map[string]string{"host": host, "role": "database"}
	return HostProfile{
		Hostname: host,
		Role:     "database",
		Metrics: []MetricProfile{
			{Name: "pg_queries_total", Type: MetricCounter, BaseVal: 500, NoiseAmp: 50, Labels: labels},
			{Name: "pg_active_connections", Type: MetricGauge, BaseVal: 30, NoiseAmp: 10, Labels: labels},
			{Name: "pg_replication_lag_bytes", Type: MetricGauge, BaseVal: 1024, NoiseAmp: 512, Labels: labels},
			{Name: "cpu_usage_percent", Type: MetricGaugeCPU, BaseVal: 35, NoiseAmp: 8, Labels: labels},
			{Name: "memory_usage_bytes", Type: MetricGaugeMemory, BaseVal: 8e9, NoiseAmp: 2e8, Labels: labels},
			{Name: "disk_io_bytes", Type: MetricCounter, BaseVal: 1e6, NoiseAmp: 5e5, Labels: labels},
		},
	}
}

func cacheProfile(host string) HostProfile {
	labels := map[string]string{"host": host, "role": "cache"}
	return HostProfile{
		Hostname: host,
		Role:     "cache",
		Metrics: []MetricProfile{
			{Name: "redis_commands_total", Type: MetricCounter, BaseVal: 2000, NoiseAmp: 200, Labels: labels},
			{Name: "redis_hit_ratio", Type: MetricGaugeRatio, BaseVal: 0.95, NoiseAmp: 0.03, Labels: labels},
			{Name: "redis_memory_bytes", Type: MetricGaugeMemory, BaseVal: 2e9, NoiseAmp: 1e8, Labels: labels},
			{Name: "redis_connected_clients", Type: MetricGauge, BaseVal: 50, NoiseAmp: 15, Labels: labels},
			{Name: "cpu_usage_percent", Type: MetricGaugeCPU, BaseVal: 20, NoiseAmp: 5, Labels: labels},
		},
	}
}

func queueProfile(host string) HostProfile {
	labels := map[string]string{"host": host, "role": "queue"}
	return HostProfile{
		Hostname: host,
		Role:     "queue",
		Metrics: []MetricProfile{
			{Name: "queue_messages_total", Type: MetricCounter, BaseVal: 1000, NoiseAmp: 100, Labels: labels},
			{Name: "queue_publish_rate", Type: MetricGauge, BaseVal: 200, NoiseAmp: 50, Labels: labels},
			{Name: "queue_consume_rate", Type: MetricGauge, BaseVal: 195, NoiseAmp: 45, Labels: labels},
			{Name: "queue_depth", Type: MetricGauge, BaseVal: 50, NoiseAmp: 30, Labels: labels},
			{Name: "cpu_usage_percent", Type: MetricGaugeCPU, BaseVal: 25, NoiseAmp: 5, Labels: labels},
			{Name: "memory_usage_bytes", Type: MetricGaugeMemory, BaseVal: 3e9, NoiseAmp: 2e8, Labels: labels},
		},
	}
}

func lbProfile(host string) HostProfile {
	labels := map[string]string{"host": host, "role": "loadbalancer"}
	return HostProfile{
		Hostname: host,
		Role:     "loadbalancer",
		Metrics: []MetricProfile{
			{Name: "lb_requests_total", Type: MetricCounter, BaseVal: 300, NoiseAmp: 50, Labels: labels},
			{Name: "lb_active_connections", Type: MetricGauge, BaseVal: 200, NoiseAmp: 60, Labels: labels},
			{Name: "lb_upstream_response_time", Type: MetricGauge, BaseVal: 0.03, NoiseAmp: 0.01, Labels: labels},
			{Name: "lb_error_rate", Type: MetricGaugeRatio, BaseVal: 0.002, NoiseAmp: 0.001, Labels: labels},
			{Name: "cpu_usage_percent", Type: MetricGaugeCPU, BaseVal: 15, NoiseAmp: 5, Labels: labels},
		},
	}
}
