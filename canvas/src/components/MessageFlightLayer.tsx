/** MessageFlightLayer — flies an envelope from the source agent to the target
 *  agent on the spatial canvas whenever a delegate / message event fires.
 *
 *  Mounted INSIDE <ReactFlow> so its ViewportPortal places the envelope in flow
 *  coordinates; it therefore pans and zooms with the canvas for free. The
 *  flight lifecycle (which events become envelopes, reduced-motion opt-out,
 *  expiry) lives in useA2AFlights — this component only resolves node centres
 *  and renders. */
import { ViewportPortal, type Node } from "@xyflow/react";
import { useCanvasStore } from "@/store/canvas";
import { useA2AFlights } from "@/hooks/useA2AFlights";
import { FlightEnvelope, type Point } from "./FlightEnvelope";
import type { WorkspaceNodeData } from "@/store/canvas";

// Fallback node footprint when React Flow has not measured a node yet. Matches
// WorkspaceNode's leaf size (w-[300px] min-h-[176px]); a slightly-off centre
// for the first frame after mount is invisible at flight scale.
const DEFAULT_W = 300;
const DEFAULT_H = 176;

function nodeCenter(n: Node<WorkspaceNodeData>): Point {
  const w = n.measured?.width ?? DEFAULT_W;
  const h = n.measured?.height ?? DEFAULT_H;
  return { x: n.position.x + w / 2, y: n.position.y + h / 2 };
}

export function MessageFlightLayer() {
  const flights = useA2AFlights();
  const nodes = useCanvasStore((s) => s.nodes);

  if (flights.length === 0) return null;

  return (
    <ViewportPortal>
      {flights.map((f) => {
        const src = nodes.find((n) => n.id === f.sourceId);
        const dst = nodes.find((n) => n.id === f.targetId);
        // Both endpoints must be on-canvas to draw a path between them.
        if (!src || !dst) return null;
        return (
          <FlightEnvelope key={f.key} from={nodeCenter(src)} to={nodeCenter(dst)} kind={f.kind} />
        );
      })}
    </ViewportPortal>
  );
}
