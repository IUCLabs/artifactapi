CREATE TABLE IF NOT EXISTS settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    s3_health_up_threshold INTEGER CHECK (s3_health_up_threshold BETWEEN 1 AND 100),
    s3_health_down_threshold INTEGER CHECK (s3_health_down_threshold BETWEEN 1 AND 100),
    s3_health_interval_ms INTEGER CHECK (s3_health_interval_ms BETWEEN 500 AND 3600000),
    s3_health_timeout_ms INTEGER CHECK (s3_health_timeout_ms BETWEEN 100 AND 3599999),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (s3_health_timeout_ms < s3_health_interval_ms)
);
