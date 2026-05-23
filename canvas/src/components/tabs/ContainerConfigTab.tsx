"use client";

import { runtimeDisplayName } from "@/lib/runtime-names";
import type { WorkspaceNodeData } from "@/store/canvas";

type Props = {
  data: Pick<
    WorkspaceNodeData,
    "runtime" | "status" | "needsRestart" | "activeTasks" | "deliveryMode"
    | "workspaceAccess" | "maxConcurrentTasks"
  >;
};

export function ContainerConfigTab({ data }: Props) {
  const runtime = data.runtime || "unknown";
  const workspaceAccess = formatAccess(data.workspaceAccess);
  const maxConcurrentTasks = data.maxConcurrentTasks ? String(data.maxConcurrentTasks) : "platform-managed";
  const mountedPath = "/workspace";
  const privilegeStatus = "standard";
  const deliveryMode = data.deliveryMode || "push";

  return (
    <div className="p-4 space-y-4">
      <section className="rounded-lg border border-line/50 bg-surface-card/40 p-4">
        <div className="mb-3">
          <h3 className="text-sm font-semibold text-ink">Container Config</h3>
        </div>

        <dl className="grid grid-cols-1 gap-2 text-[11px]">
          <ConfigRow label="Runtime image" value={runtimeDisplayName(runtime)} detail={runtime} />
          <ConfigRow label="Workspace access" value={workspaceAccess} />
          <ConfigRow label="Max concurrent tasks" value={maxConcurrentTasks} />
          <ConfigRow label="Mounted workspace path" value={mountedPath} />
          <ConfigRow label="Container privileges" value={privilegeStatus} />
          <ConfigRow label="Delivery mode" value={deliveryMode} />
        </dl>
      </section>

      <section className="rounded-lg border border-line/50 bg-surface-card/40 p-4">
        <h3 className="mb-3 text-sm font-semibold text-ink">Session Controls</h3>
        <div className="grid grid-cols-2 gap-2">
          <ReadOnlyAction label={data.needsRestart ? "Restart required" : "Restart"} />
          <ReadOnlyAction label="Reset session" />
        </div>
      </section>

      <section className="rounded-lg border border-line/50 bg-surface-card/40 p-4">
        <h3 className="mb-3 text-sm font-semibold text-ink">Status</h3>
        <dl className="grid grid-cols-1 gap-2 text-[11px]">
          <ConfigRow label="Container status" value={data.status} />
          <ConfigRow label="Active tasks" value={String(data.activeTasks ?? 0)} />
          <ConfigRow label="Mounted path access" value="available" />
        </dl>
      </section>
    </div>
  );
}

function formatAccess(value: string | null | undefined): string {
  if (!value) return "none";
  return value.replace(/_/g, "-");
}

function ConfigRow({
  label,
  value,
  detail,
}: {
  label: string;
  value: string;
  detail?: string;
}) {
  return (
    <div className="flex items-start justify-between gap-3 rounded-md bg-surface-sunken/40 px-3 py-2">
      <dt className="text-ink-mid">{label}</dt>
      <dd className="min-w-0 text-right">
        <div className="font-mono text-ink break-words">{value}</div>
        {detail && detail !== value && (
          <div className="mt-0.5 font-mono text-[10px] text-ink-mid break-words">{detail}</div>
        )}
      </dd>
    </div>
  );
}

function ReadOnlyAction({ label }: { label: string }) {
  return (
    <button
      type="button"
      disabled
      className="rounded-md border border-line/50 bg-surface-sunken/40 px-3 py-2 text-[11px] text-ink-mid disabled:cursor-not-allowed disabled:opacity-70"
    >
      {label}
    </button>
  );
}
