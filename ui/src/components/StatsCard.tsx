import './StatsCard.css';

interface StatsCardProps {
  label: string;
  value: string | number;
  sub?: string;
}

export function StatsCard({ label, value, sub }: StatsCardProps) {
  return (
    <div className="stats-card">
      <div className="stats-label">{label}</div>
      <div className="stats-value">{value}</div>
      {sub && <div className="stats-sub">{sub}</div>}
    </div>
  );
}
