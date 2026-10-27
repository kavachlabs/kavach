import { useEffect, useState } from "react";
import "./RevenueChart.css";

type Point = { month: string; revenue: number };

export function RevenueChart() {
  const [points, setPoints] = useState<Point[]>([]);
  useEffect(() => {
    fetch("/api/revenue").then((r) => r.json()).then(setPoints);
  }, []);
  const max = Math.max(...points.map((p) => p.revenue));
  return (
    <div className="chart">
      {points.map((p) => (
        <div key={p.month} className="bar" style={{ height: `${(p.revenue / max) * 100}%` }}>
          <span className="label">{p.month}</span>
        </div>
      ))}
    </div>
  );
}
