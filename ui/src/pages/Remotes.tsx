import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { Remote } from '../api/types';
import { Badge } from '../components/Badge';
import { DataTable } from '../components/DataTable';
import './Remotes.css';

const typeColors: Record<string, 'blue' | 'green' | 'yellow' | 'red' | 'default'> = {
  docker: 'blue',
  helm: 'green',
  rpm: 'yellow',
  pypi: 'blue',
  npm: 'red',
  generic: 'default',
  alpine: 'green',
  puppet: 'yellow',
  terraform: 'blue',
  goproxy: 'green',
};

export function Remotes() {
  const navigate = useNavigate();
  const [remotes, setRemotes] = useState<Remote[]>([]);
  const [filter, setFilter] = useState('');
  const [typeFilter, setTypeFilter] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.listRemotes()
      .then(r => setRemotes(r || []))
      .finally(() => setLoading(false));
  }, []);

  const remoteOnly = remotes.filter(r => r.repo_type !== 'local');
  const types = [...new Set(remoteOnly.map(r => r.package_type))].sort();

  const filtered = remoteOnly.filter(r => {
    if (typeFilter && r.package_type !== typeFilter) return false;
    if (filter && !r.name.toLowerCase().includes(filter.toLowerCase())) return false;
    return true;
  });

  return (
    <div>
      <h1 className="page-title">Remotes</h1>

      <div className="remotes-toolbar">
        <input
          className="search-input"
          placeholder="Filter by name..."
          value={filter}
          onChange={e => setFilter(e.target.value)}
        />
        <select
          className="type-select"
          value={typeFilter}
          onChange={e => setTypeFilter(e.target.value)}
        >
          <option value="">All types</option>
          {types.map(t => (
            <option key={t} value={t}>{t}</option>
          ))}
        </select>
        <span className="result-count">{filtered.length} remotes</span>
      </div>

      {loading ? (
        <div className="loading">Loading...</div>
      ) : (
        <DataTable
          columns={[
            {
              key: 'name',
              header: 'Name',
              render: (r: Remote) => <span className="mono">{r.name}</span>,
            },
            {
              key: 'type',
              header: 'Type',
              render: (r: Remote) => (
                <Badge variant={typeColors[r.package_type] || 'default'}>
                  {r.package_type}
                </Badge>
              ),
              width: '110px',
            },
            {
              key: 'description',
              header: 'Description',
              render: (r: Remote) => r.description || <span className="text-muted">—</span>,
            },
            {
              key: 'ttl',
              header: 'Mutable TTL',
              render: (r: Remote) => <span className="mono">{r.mutable_ttl}s</span>,
              width: '110px',
            },
            {
              key: 'managed',
              header: 'Managed',
              render: (r: Remote) =>
                r.managed_by ? <Badge variant="blue">{r.managed_by}</Badge> : <span className="text-muted">—</span>,
              width: '100px',
            },
          ]}
          data={filtered}
          emptyMessage="No remotes match"
          onRowClick={(r) => navigate(`/remotes/${r.name}`)}
        />
      )}
    </div>
  );
}
