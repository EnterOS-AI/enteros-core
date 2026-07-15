# Communication Rules

The hierarchy IS the topology. There is no manual connection wiring — communication is derived automatically from the parent/child structure.

## The Rules

| Direction | Allowed? | Example |
|-----------|----------|---------|
| Same non-root-parent sibling <-> sibling | Yes | Marketing <-> Developer PM |
| Ancestor -> descendant (any depth) | Yes | Business Core -> Frontend Agent |
| Descendant -> ancestor (any depth) | Yes | Auto Test -> Developer PM |
| Unrelated root <-> unrelated root | No | Tenant A root -> Tenant B root |
| Disjoint subtrees | No | Frontend Agent -> Operations |

## Visual Example

```
Business Core
+-- Marketing          <--can talk--> Developer PM
+-- Developer PM       <--can talk--> Operations
|   +-- Frontend       <--can talk--> Backend
|   +-- Backend        <--can talk--> QA PM
|   +-- QA PM
|       +-- Auto Test  <--can talk--> Manual Review
|       +-- Manual Review
+-- Operations
```

- Developer PM can talk to Marketing and Operations (same-parent siblings),
  and to every descendant in its own subtree.
- Frontend can talk to Backend and QA PM (same-parent siblings) and to its
  ancestors Developer PM and Business Core.
- Frontend **cannot** talk to Auto Test, Manual Review, Marketing, or
  Operations because those are in disjoint subtrees and neither side is the
  other's ancestor.
- Auto Test can talk to Manual Review (same-parent sibling) and to its
  ancestors QA PM, Developer PM, and Business Core.

## Access Check

The platform validates every discovery request with a hierarchy check:

```go
func CanCommunicate(callerID, targetID string) bool {
    if callerID == targetID {
        return true
    }

    caller := db.GetWorkspace(callerID)
    target := db.GetWorkspace(targetID)

    // siblings — only a shared non-root parent establishes a relationship
    if caller.ParentID != nil && target.ParentID != nil &&
       *caller.ParentID == *target.ParentID {
        return true
    }

    // either side may be anywhere on the other's parent chain
    if isAncestorOf(caller.ID, target.ID) ||
       isAncestorOf(target.ID, caller.ID) {
        return true
    }

    return false
}
```

`GET /registry/discover/:id` requires `X-Workspace-ID`, validates the discovery
credential for that caller, runs `CanCommunicate()`, and returns **403
Forbidden** if the caller is not allowed. Database lookup failures deny rather
than granting access. The production ancestor walk is bounded to 32 steps so a
malformed parent cycle cannot loop forever.

## Peer Discovery

Instead of a connections table, the platform derives reachable workspaces from the hierarchy:

```
GET /registry/:id/peers
```

Returns the direct topology view: same-parent siblings, direct children, and
the direct parent. `CanCommunicate()` additionally permits distant
ancestor/descendant pairs, but this list endpoint does not recursively enumerate
them.

```python
async def get_reachable_workspaces(workspace_id: str) -> list:
    ws = db.GetWorkspace(workspace_id)
    reachable = []

    # siblings — same parent
    if ws.parent_id:
        siblings = db.GetChildren(ws.parent_id)
        reachable += [s for s in siblings if s.id != workspace_id]

    # children — own sub-workspaces
    children = db.GetChildren(workspace_id)
    reachable += children

    # parent — can talk up
    if ws.parent_id:
        parent = db.GetWorkspace(ws.parent_id)
        reachable.append(parent)

    return reachable
```

## What This Replaces

The hierarchy-based model removes several components:

| Removed | Replaced by |
|---------|-------------|
| `workspace_connections` table | `parent_id` on `workspaces` table |
| Dedicated topology or expand/collapse events | Authenticated workspace creation and `PATCH /workspaces/:id` updates to `parent_id`; clients then rehydrate current rows |
| `/topology/connect` endpoint | Nesting via drag-into on canvas |
| Canvas edge drawing UI | Edges auto-rendered from hierarchy |
| Workspace whitelist table | `CanCommunicate()` hierarchy check |
| Bundle connection definitions | Bundle `sub_workspaces` array |

## Canvas Behavior

- **No edge drawing.** Users don't wire workspaces — they **nest** them
- Edges render **automatically** from parent/child relationships
- The visual is a true **org chart**, not a flowchart
- Dragging a workspace **inside** another workspace nests it as a sub-workspace

## Why This Is Better

The org chart IS the access control policy. Simpler schema, simpler security, simpler canvas. No configuration drift between "who should talk to whom" and "who can talk to whom."

## Related Docs

- [Platform API — Team hierarchy](./platform-api.md#team-hierarchy) — Current nesting and visual-collapse surfaces
- [System Prompt Structure](../agent-runtime/system-prompt-structure.md) — How peer capabilities are injected
- [A2A Protocol](./a2a-protocol.md) — Discovery flow
- [Platform API](./platform-api.md) — Endpoint reference
