package performance

import "time"

const (
	DefaultWindowSeconds  = 300
	DefaultMinSamples     = 5
	DefaultEWMAAlpha      = 0.25
	DefaultWeightTPS      = 1.0
	DefaultWeightLatency  = 0.25
	DefaultWeightFailure  = 2.0
	DefaultWeightInflight = 0.5
	minWindowSeconds      = 1
	maxWindowSeconds      = 24 * 60 * 60
	minEWMAAlpha          = 0.01
	maxEWMAAlpha          = 1.0
)

type Config struct {
	Enabled        bool
	ShadowLog      bool
	Window         time.Duration
	MinSamples     int64
	EWMAAlpha      float64
	WeightTPS      float64
	WeightLatency  float64
	WeightFailure  float64
	WeightInflight float64
}

func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		ShadowLog:      true,
		Window:         time.Duration(DefaultWindowSeconds) * time.Second,
		MinSamples:     DefaultMinSamples,
		EWMAAlpha:      DefaultEWMAAlpha,
		WeightTPS:      DefaultWeightTPS,
		WeightLatency:  DefaultWeightLatency,
		WeightFailure:  DefaultWeightFailure,
		WeightInflight: DefaultWeightInflight,
	}
}

func NormalizeConfig(cfg Config) Config {
	def := DefaultConfig()
	if cfg.Window <= 0 {
		cfg.Window = def.Window
	}
	minWindow := time.Duration(minWindowSeconds) * time.Second
	maxWindow := time.Duration(maxWindowSeconds) * time.Second
	if cfg.Window < minWindow {
		cfg.Window = minWindow
	}
	if cfg.Window > maxWindow {
		cfg.Window = maxWindow
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = def.MinSamples
	}
	if cfg.EWMAAlpha <= 0 {
		cfg.EWMAAlpha = def.EWMAAlpha
	}
	if cfg.EWMAAlpha < minEWMAAlpha {
		cfg.EWMAAlpha = minEWMAAlpha
	}
	if cfg.EWMAAlpha > maxEWMAAlpha {
		cfg.EWMAAlpha = maxEWMAAlpha
	}
	if cfg.WeightTPS == 0 {
		cfg.WeightTPS = def.WeightTPS
	}
	if cfg.WeightLatency == 0 {
		cfg.WeightLatency = def.WeightLatency
	}
	if cfg.WeightFailure == 0 {
		cfg.WeightFailure = def.WeightFailure
	}
	if cfg.WeightInflight == 0 {
		cfg.WeightInflight = def.WeightInflight
	}
	return cfg
}

type Key struct {
	Provider string
	AuthID   string
	Model    string
}

type Sample struct {
	Provider         string
	AuthID           string
	AuthIndex        string
	Model            string
	RequestedAt      time.Time
	Latency          time.Duration
	FirstByteLatency time.Duration
	OutputTokens     int64
	TotalTokens      int64
	Failed           bool
	StatusCode       int
}

type Snapshot struct {
	Provider           string    `json:"provider"`
	AuthID             string    `json:"auth_id"`
	AuthIndex          string    `json:"auth_index,omitempty"`
	Model              string    `json:"model"`
	RequestCount       int64     `json:"request_count"`
	SuccessCount       int64     `json:"success_count"`
	FailureCount       int64     `json:"failure_count"`
	OutputTokens       int64     `json:"output_tokens"`
	OutputTPSEWMA      float64   `json:"output_tps_ewma"`
	LatencyMsEWMA      float64   `json:"latency_ms_ewma"`
	FirstByteMsEWMA    float64   `json:"first_byte_ms_ewma,omitempty"`
	FailureRateEWMA    float64   `json:"failure_rate_ewma"`
	WindowRequestCount int64     `json:"window_request_count"`
	WindowOutputTokens int64     `json:"window_output_tokens"`
	WindowOutputTPS    float64   `json:"window_output_tps"`
	SampleReady        bool      `json:"sample_ready"`
	LastSeen           time.Time `json:"last_seen"`
}

type SnapshotFilter struct {
	Provider  string
	Model     string
	AuthID    string
	ReadyOnly bool
}
