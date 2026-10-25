export interface Remote {
  name: string;
  package_type: string;
  repo_type: string;
  base_url: string;
  description: string;
  username?: string;
  immutable_ttl: number;
  mutable_ttl: number;
  check_mutable: boolean;
  patterns: string[];
  blocklist: string[];
  mutable_patterns: string[];
  immutable_patterns: string[];
  ban_tags_enabled: boolean;
  ban_tags: string[];
  quarantine_enabled: boolean;
  quarantine_days: number;
  stale_on_error: boolean;
  serve_stale: boolean;
  releases_remote: string;
  managed_by: string;
  created_at: string;
  updated_at: string;
}

export interface Virtual {
  name: string;
  package_type: string;
  description: string;
  members: string[];
  managed_by: string;
  created_at: string;
  updated_at: string;
}

export interface Artifact {
  id: number;
  remote_name: string;
  path: string;
  content_hash: string;
  upstream_etag: string;
  first_seen_at: string;
  last_fetched_at: string;
  last_accessed_at: string;
  fetch_count: number;
  access_count: number;
  size_bytes: number;
  content_type: string;
}

export interface OverviewStats {
  total_remotes: number;
  total_objects: number;
  total_bytes: number;
  total_blobs_deduped: number;
  bandwidth_saved_30d: number;
}

export interface RemoteStatRow {
  name: string;
  object_count: number;
  total_bytes: number;
  requests_30d: number;
}

export interface FileStatRow {
  remote_name: string;
  path: string;
  access_count: number;
  size_bytes: number;
}

export interface BandwidthStatRow {
  remote_name: string;
  path: string;
  bandwidth: number;
  requests: number;
}

export interface HealthStatus {
  status: string;
  postgres: string;
  redis: string;
  s3: string;
}

export interface ProbeResult {
  status: number;
  source: string;
  content_type: string;
  size_bytes: number;
  headers: Record<string, string>;
  duration_ms: number;
  error: string;
}
