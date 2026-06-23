import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Remote, Virtual } from '../api/types';
import { Badge } from '../components/Badge';
import { DataTable } from '../components/DataTable';
import './Virtuals.css';

export function Virtuals() {
  const [virtuals, setVirtuals] = useState<Virtual[]>([]);
  const [remoteMap, setRemoteMap] = useState<Record<string, Remote>>({});
  const [loading, setLoading] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);

  useEffect(() => {
    Promise.all([api.listVirtuals(), api.listRemotes()])
      .then(([v, r]) => {
        setVirtuals(v || []);
        const map: Record<string, Remote> = {};
        for (const remote of r || []) {
          map[remote.name] = remote;
        }
        setRemoteMap(map);
      })
      .finally(() => setLoading(false));
  }, []);

  function memberLink(name: string) {
    const remote = remoteMap[name];
    if (remote?.repo_type === 'local') {
      return `/locals/${name}`;
    }
    return `/remotes/${name}`;
  }

  return (
    <div>
      <h1 className="page-title">Virtual Repositories</h1>

      {loading ? (
        <div className="loading">Loading...</div>
      ) : (
        <DataTable
          columns={[
            {
              key: 'name',
              header: 'Name',
              render: (v: Virtual) => <span className="mono">{v.name}</span>,
            },
            {
              key: 'type',
              header: 'Type',
              render: (v: Virtual) => <Badge variant="green">{v.package_type}</Badge>,
              width: '110px',
            },
            {
              key: 'members',
              header: 'Members',
              render: (v: Virtual) => (
                <span className="member-count">{v.members?.length || 0} repos</span>
              ),
              width: '110px',
            },
            {
              key: 'description',
              header: 'Description',
              render: (v: Virtual) => v.description || <span className="text-muted">—</span>,
            },
            {
              key: 'managed',
              header: 'Managed',
              render: (v: Virtual) =>
                v.managed_by ? <Badge variant="blue">{v.managed_by}</Badge> : <span className="text-muted">—</span>,
              width: '100px',
            },
          ]}
          data={virtuals}
          emptyMessage="No virtual repositories configured"
          onRowClick={(v) => setExpanded(expanded === v.name ? null : v.name)}
        />
      )}

      {expanded && (
        <div className="virtual-detail-panel">
          <h3 className="section-label">Members of {expanded}</h3>
          <ul className="member-list">
            {virtuals
              .find(v => v.name === expanded)
              ?.members?.map((m, i) => {
                const remote = remoteMap[m];
                const typeLabel = remote?.repo_type === 'local' ? 'local' : 'remote';
                return (
                  <li key={m}>
                    <span className="member-priority">{i + 1}</span>
                    <Link to={memberLink(m)} className="mono">{m}</Link>
                    <Badge variant={typeLabel === 'local' ? 'yellow' : 'default'}>{typeLabel}</Badge>
                  </li>
                );
              })}
          </ul>
        </div>
      )}
    </div>
  );
}
