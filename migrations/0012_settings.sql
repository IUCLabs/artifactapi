CREATE TABLE IF NOT EXISTS settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    s3_health_up_threshold INTEGER CHECK (s3_health_up_threshold >= 1),
    s3_health_down_threshold INTEGER CHECK (s3_health_down_threshold >= 1),
    s3_health_interval_ms INTEGER CHECK (s3_health_interval_ms >= 1),
    s3_health_timeout_ms INTEGER CHECK (s3_health_timeout_ms >= 1),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
