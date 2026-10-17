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
  cargo: 'yellow',
  nuget: 'blue',
};

export function Locals() {
  const navigate = useNavigate();
  const [remotes, setRemotes] = useState<Remote[]>([]);
  const [filter, setFilter] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.listRemotes()
      .then(r => setRemotes((r || []).filter(x => x.repo_type === 'local')))
      .finally(() => setLoading(false));
  }, []);

  const filtered = remotes.filter(r => {
    if (filter && !r.name.toLowerCase().includes(filter.toLowerCase())) return false;
    return true;
  });

  return (
    <div>
      <h1 className="page-title">Local Repositories</h1>

      <div className="remotes-toolbar">
        <input
          className="search-input"
          placeholder="Filter by name..."
          value={filter}
          onChange={e => setFilter(e.target.value)}
        />
        <span className="result-count">{filtered.length} locals</span>
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
              key: 'managed',
              header: 'Managed',
              render: (r: Remote) =>
                r.managed_by ? <Badge variant="blue">{r.managed_by}</Badge> : <span className="text-muted">—</span>,
              width: '100px',
            },
          ]}
          data={filtered}
          emptyMessage="No local repositories configured"
          onRowClick={(r) => navigate(`/locals/${r.name}`)}
        />
      )}
    </div>
  );
}
