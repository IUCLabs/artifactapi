import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { OverviewStats, RemoteStatRow, FileStatRow, BandwidthStatRow, HealthStatus } from '../api/types';
import { StatsCard } from '../components/StatsCard';
import { Badge } from '../components/Badge';
import { DataTable } from '../components/DataTable';
import { formatBytes, formatNumber } from '../components/format';
import './Dashboard.css';

export function Dashboard() {
  const [stats, setStats] = useState<OverviewStats | null>(null);
  const [topRemotes, setTopRemotes] = useState<RemoteStatRow[]>([]);
  const [topFilesByHits, setTopFilesByHits] = useState<FileStatRow[]>([]);
  const [topFilesByBW, setTopFilesByBW] = useState<BandwidthStatRow[]>([]);
  const [health, setHealth] = useState<HealthStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    Promise.all([
      api.stats(),
      api.topRemotes(),
      api.topFilesByHits(),
      api.topFilesByBandwidth(),
      api.health(),
    ])
      .then(([s, tr, tfh, tfb, h]) => {
        setStats(s);
        setTopRemotes(tr || []);
        setTopFilesByHits(tfh || []);
        setTopFilesByBW(tfb || []);
        setHealth(h);
      })
      .catch(e => setError(e.message));
  }, []);

  if (error) return <div className="error-banner">{error}</div>;
  if (!stats) return <div className="loading">Loading...</div>;

  return (
    <div>
      <h1 className="page-title">Dashboard</h1>

      <div className="stats-row">
        <StatsCard label="Remotes" value={formatNumber(stats.total_remotes)} />
        <StatsCard label="Cached Objects" value={formatNumber(stats.total_objects)} />
        <StatsCard label="Storage Used" value={formatBytes(stats.total_bytes)} />
        <StatsCard
          label="Dedup Savings"
          value={formatNumber(stats.total_blobs_deduped)}
          sub="shared blobs"
        />
        <StatsCard
          label="Bandwidth Saved"
          value={formatBytes(stats.bandwidth_saved_30d)}
          sub="last 30 days"
        />
      </div>

      {health && (
        <div className="health-row">
          <span className="health-label">Services</span>
          <Badge variant={health.postgres === 'ok' ? 'green' : 'red'}>
            postgres: {health.postgres}
          </Badge>
          <Badge variant={health.redis === 'ok' ? 'green' : 'red'}>
            redis: {health.redis}
          </Badge>
          <Badge variant={health.s3 === 'ok' ? 'green' : health.s3 === 'unknown' ? 'yellow' : 'red'}>
            s3: {health.s3}
          </Badge>
        </div>
      )}

      <h2 className="section-title">Top Remotes by Size</h2>
      <DataTable
        columns={[
          {
            key: 'name',
            header: 'Name',
            render: (r: RemoteStatRow) => <Link to={`/remotes/${r.name}`}>{r.name}</Link>,
          },
          {
            key: 'objects',
            header: 'Objects',
            render: (r: RemoteStatRow) => formatNumber(r.object_count),
            width: '120px',
          },
          {
            key: 'size',
            header: 'Size',
            render: (r: RemoteStatRow) => formatBytes(r.total_bytes),
            width: '120px',
          },
          {
            key: 'requests',
            header: 'Requests (30d)',
            render: (r: RemoteStatRow) => formatNumber(r.requests_30d),
            width: '140px',
          },
        ]}
        data={topRemotes}
        emptyMessage="No remotes configured yet"
      />

      <div className="top-files-grid">
        <div>
          <h2 className="section-title">Top Files by Hits</h2>
          <DataTable
            columns={[
              {
                key: 'path',
                header: 'File',
                render: (r: FileStatRow) => (
                  <Link to={`/remotes/${r.remote_name}/objects`} className="file-link">
                    <span className="file-remote">{r.remote_name}</span>
                    <span className="file-path mono">{r.path}</span>
                  </Link>
                ),
              },
              {
                key: 'hits',
                header: 'Hits',
                render: (r: FileStatRow) => formatNumber(r.access_count),
                width: '90px',
              },
              {
                key: 'size',
                header: 'Size',
                render: (r: FileStatRow) => formatBytes(r.size_bytes),
                width: '100px',
              },
            ]}
            data={topFilesByHits}
            emptyMessage="No cached files yet"
          />
        </div>

        <div>
          <h2 className="section-title">Top Files by Bandwidth (30d)</h2>
          <DataTable
            columns={[
              {
                key: 'path',
                header: 'File',
                render: (r: BandwidthStatRow) => (
                  <Link to={`/remotes/${r.remote_name}/objects`} className="file-link">
                    <span className="file-remote">{r.remote_name}</span>
                    <span className="file-path mono">{r.path}</span>
                  </Link>
                ),
              },
              {
                key: 'bandwidth',
                header: 'Bandwidth',
                render: (r: BandwidthStatRow) => formatBytes(r.bandwidth),
                width: '110px',
              },
              {
                key: 'requests',
                header: 'Requests',
                render: (r: BandwidthStatRow) => formatNumber(r.requests),
                width: '100px',
              },
            ]}
            data={topFilesByBW}
            emptyMessage="No access data yet"
          />
        </div>
      </div>
    </div>
  );
}
