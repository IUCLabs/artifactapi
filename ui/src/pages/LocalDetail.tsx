import { useEffect, useState } from 'react';
import { useParams, Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Remote } from '../api/types';
import { Badge } from '../components/Badge';
import { UsageInstructions } from '../components/UsageInstructions';
import './RemoteDetail.css';

export function LocalDetail() {
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
        <Link to="/locals" className="back-link">&larr; Locals</Link>
        <h1 className="page-title">{remote.name}</h1>
        <div className="detail-badges">
          <Badge variant="blue">{remote.package_type}</Badge>
          <Badge variant="default">local</Badge>
          {remote.managed_by && <Badge variant="green">managed by {remote.managed_by}</Badge>}
        </div>
      </div>

      {remote.description && (
        <p className="detail-description">{remote.description}</p>
      )}

      <UsageInstructions packageType={remote.package_type} repoClass="local" name={remote.name} />

      <div className="detail-actions">
        <Link to={`/locals/${remote.name}/objects`} className="btn btn-primary">
          Browse Files
        </Link>
      </div>
    </div>
  );
}
