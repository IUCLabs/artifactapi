import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { OverviewStats, RemoteStatRow, HealthStatus } from '../api/types';
import { StatsCard } from '../components/StatsCard';
import { Badge } from '../components/Badge';
import { DataTable } from '../components/DataTable';
import { formatBytes, formatNumber } from '../components/format';
import './Dashboard.css';

export function Dashboard() {
  const [stats, setStats] = useState<OverviewStats | null>(null);
  const [topRemotes, setTopRemotes] = useState<RemoteStatRow[]>([]);
  const [health, setHealth] = useState<HealthStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    Promise.all([api.stats(), api.topRemotes(), api.health()])
      .then(([s, tr, h]) => {
        setStats(s);
        setTopRemotes(tr || []);
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
          <Badge variant={health.s3 === 'ok' ? 'green' : 'red'}>
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
    </div>
  );
}
