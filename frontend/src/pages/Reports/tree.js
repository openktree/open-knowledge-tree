// Build a nested tree from a flat list of reports.
// Each node: { ...report, children: [node...] }.
// Top-level nodes are those whose parent_id is falsy (null/empty).
// Siblings are ordered by created_at ascending (oldest first) so
// the meta-synthesis created later appears below its children.

export function buildReportTree(reports) {
  const byParent = new Map();
  const byId = new Map();
  for (const r of reports) {
    byId.set(r.id, r);
    const key = r.parent_id || "root";
    if (!byParent.has(key)) byParent.set(key, []);
    byParent.get(key).push(r);
  }
  for (const arr of byParent.values()) {
    arr.sort((a, b) => (a.created_at || "").localeCompare(b.created_at || ""));
  }
  const roots = byParent.get("root") || [];
  return roots.map((r) => attachChildren(r, byParent));
}

function attachChildren(report, byParent) {
  const children = (byParent.get(report.id) || []).map((c) => attachChildren(c, byParent));
  return { ...report, children };
}

// Flatten a tree into a list of { report, depth, hasChildren }
// rows, expanding only the nodes whose id is in `expandedIds`.
export function flattenTree(roots, expandedIds) {
  const out = [];
  const walk = (nodes, depth) => {
    for (const node of nodes) {
      const hasChildren = node.children && node.children.length > 0;
      out.push({ report: node, depth, hasChildren });
      if (hasChildren && expandedIds.has(node.id)) {
        walk(node.children, depth + 1);
      }
    }
  };
  walk(roots, 0);
  return out;
}