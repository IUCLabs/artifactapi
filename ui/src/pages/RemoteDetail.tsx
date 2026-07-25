import { useEffect, useState } from 'react';
import { useParams, Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Remote } from '../api/types';
import { Badge } from '../components/Badge';
import { UsageInstructions } from '../components/UsageInstructions';
import './RemoteDetail.css';

export function RemoteDetail() {
  const { name } = useParams<{ name: string }>();
  const [remote, setRemote] = useState<Remote | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!name) return;
    api.getRemote(name)
      .then(setRemote)
      .catch(e => setError(e.message));
  }, [name]);

  if (error) return <div className="error-banner">{error}</div>;
  if (!remote) return <div className="loading">Loading...</div>;

  return (
    <div>
      <div className="detail-header">
        <Link to="/remotes" className="back-link">&larr; Remotes</Link>
        <h1 className="page-title">{remote.name}</h1>
        <div className="detail-badges">
          <Badge variant="blue">{remote.package_type}</Badge>
          {remote.managed_by && <Badge variant="green">managed by {remote.managed_by}</Badge>}
        </div>
      </div>

      {remote.description && (
        <p className="detail-description">{remote.description}</p>
      )}

      <div className="detail-grid">
        <div className="detail-section">
          <h3 className="section-label">Configuration</h3>
          <dl className="detail-dl">
            <dt>Base URL</dt>
            <dd className="mono">{remote.base_url}</dd>
            <dt>Immutable TTL</dt>
            <dd className="mono">{remote.immutable_ttl === 0 ? 'forever' : `${remote.immutable_ttl}s`}</dd>
            <dt>Mutable TTL</dt>
            <dd className="mono">{remote.mutable_ttl}s</dd>
            <dt>Conditional Revalidation</dt>
            <dd>{remote.check_mutable ? 'enabled' : 'disabled'}</dd>
            <dt>Stale on Error</dt>
            <dd>{remote.stale_on_error ? 'enabled' : 'disabled'}</dd>
            {remote.releases_remote && (
              <>
                <dt>Releases Remote</dt>
                <dd>
                  <Link to={`/remotes/${remote.releases_remote}`} className="mono">
                    {remote.releases_remote}
                  </Link>
                </dd>
              </>
            )}
          </dl>
        </div>

        <div className="detail-section">
          <h3 className="section-label">Access Control</h3>
          <dl className="detail-dl">
            <dt>Patterns</dt>
            <dd>
              {remote.patterns?.length
                ? <PatternList patterns={remote.patterns} />
                : <span className="text-muted">none (proxy all)</span>}
            </dd>
            <dt>Blocklist</dt>
            <dd>
              {remote.blocklist?.length
                ? <PatternList patterns={remote.blocklist} />
                : <span className="text-muted">none</span>}
            </dd>
          </dl>
        </div>

        <div className="detail-section">
          <h3 className="section-label">Classification Overrides</h3>
          <dl className="detail-dl">
            <dt>Mutable</dt>
            <dd>
              {remote.mutable_patterns?.length
                ? <PatternList patterns={remote.mutable_patterns} />
                : <span className="text-muted">provider defaults only</span>}
            </dd>
            <dt>Immutable</dt>
            <dd>
              {remote.immutable_patterns?.length
                ? <PatternList patterns={remote.immutable_patterns} />
                : <span className="text-muted">provider defaults only</span>}
            </dd>
          </dl>
        </div>

        {remote.ban_tags_enabled && (
          <div className="detail-section">
            <h3 className="section-label">Tag Banning</h3>
            <dl className="detail-dl">
              <dt>Banned Tags</dt>
              <dd><PatternList patterns={remote.ban_tags || []} /></dd>
            </dl>
          </div>
        )}
      </div>

      <UsageInstructions packageType={remote.package_type} repoClass="remote" name={remote.name} />

      <div className="detail-actions">
        <Link to={`/remotes/${remote.name}/objects`} className="btn btn-primary">
          Browse Objects
        </Link>
      </div>
    </div>
  );
}

function PatternList({ patterns }: { patterns: string[] }) {
  return (
    <ul className="pattern-list">
      {patterns.map((p, i) => (
        <li key={i} className="mono">{p}</li>
      ))}
    </ul>
  );
}
