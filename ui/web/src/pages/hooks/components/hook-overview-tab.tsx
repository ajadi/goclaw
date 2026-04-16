import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import type { HookConfig } from "@/hooks/use-hooks";
import { ScriptEditor } from "./script-editor";
import { SystemBadge } from "./system-badge";

interface HookOverviewTabProps {
  hook: HookConfig;
}

const HANDLER_COLORS: Record<string, string> = {
  command: "bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300",
  http: "bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-300",
  prompt: "bg-pink-100 text-pink-700 dark:bg-pink-900/30 dark:text-pink-300",
  script: "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300",
};

const EVENT_COLORS: Record<string, string> = {
  session_start: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300",
  user_prompt_submit: "bg-violet-100 text-violet-700 dark:bg-violet-900/30 dark:text-violet-300",
  pre_tool_use: "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300",
  post_tool_use: "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300",
  stop: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300",
  subagent_start: "bg-cyan-100 text-cyan-700 dark:bg-cyan-900/30 dark:text-cyan-300",
  subagent_stop: "bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-300",
};

function MetaItem({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1">
      <p className="text-2xs uppercase tracking-wide text-muted-foreground">{label}</p>
      <div className="text-sm">{children}</div>
    </div>
  );
}

export function HookOverviewTab({ hook }: HookOverviewTabProps) {
  const { t } = useTranslation("hooks");
  const cfg = hook.config as Record<string, unknown> | undefined;
  const scriptSource = hook.handler_type === "script" ? ((cfg?.source as string) ?? "") : "";

  return (
    <div className="space-y-6">
      {/* Header card — primary identity + status */}
      <div className="rounded-lg border bg-card p-4">
        <div className="flex flex-wrap items-center gap-2">
          <span
            className={`inline-flex items-center rounded px-2 py-0.5 text-xs font-medium ${EVENT_COLORS[hook.event] ?? "bg-muted text-muted-foreground"}`}
          >
            {hook.event}
          </span>
          <span
            className={`inline-flex items-center rounded px-2 py-0.5 text-xs font-medium ${HANDLER_COLORS[hook.handler_type] ?? "bg-muted text-muted-foreground"}`}
          >
            {hook.handler_type}
          </span>
          <Badge variant="outline">{hook.scope}</Badge>
          {hook.source === "builtin" && <SystemBadge />}
          <div className="flex-1" />
          <span
            className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium ${
              hook.enabled
                ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
                : "bg-muted text-muted-foreground"
            }`}
          >
            <span className={`h-1.5 w-1.5 rounded-full ${hook.enabled ? "bg-emerald-500" : "bg-muted-foreground"}`} />
            {hook.enabled ? "Enabled" : "Disabled"}
          </span>
        </div>
        {hook.matcher && (
          <div className="mt-3 space-y-1">
            <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("table.matcher")}</p>
            <code className="block rounded bg-muted px-2 py-1 text-xs font-mono">{hook.matcher}</code>
          </div>
        )}
        {hook.if_expr && (
          <div className="mt-3 space-y-1">
            <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("form.ifExpr")}</p>
            <code className="block rounded bg-muted px-2 py-1 text-xs font-mono">{hook.if_expr}</code>
          </div>
        )}
      </div>

      {/* Metadata grid */}
      <div className="rounded-lg border bg-card p-4">
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <MetaItem label={t("form.timeout")}>
            <span className="font-mono">{hook.timeout_ms}ms</span>
          </MetaItem>
          <MetaItem label={t("form.onTimeout")}>
            <Badge variant="outline" className="capitalize">{hook.on_timeout}</Badge>
          </MetaItem>
          <MetaItem label={t("form.priority")}>
            <span className="font-mono">{hook.priority}</span>
          </MetaItem>
          <MetaItem label="Source">
            <Badge variant="secondary" className="capitalize">{hook.source}</Badge>
          </MetaItem>
          <MetaItem label="Version">
            <span className="font-mono">v{hook.version}</span>
          </MetaItem>
          <MetaItem label="Created">
            <span className="text-muted-foreground">{new Date(hook.created_at).toLocaleString()}</span>
          </MetaItem>
          <MetaItem label="Updated">
            <span className="text-muted-foreground">{new Date(hook.updated_at).toLocaleString()}</span>
          </MetaItem>
        </div>
      </div>

      {/* Handler-specific content */}
      {hook.handler_type === "script" && scriptSource && (
        <div className="space-y-2">
          <p className="text-xs font-medium text-muted-foreground">{t("form.scriptSource")}</p>
          <ScriptEditor value={scriptSource} onChange={() => {}} readOnly minLines={Math.min(20, scriptSource.split("\n").length + 1)} />
        </div>
      )}

      {hook.handler_type === "http" && cfg && <HttpConfigCard cfg={cfg} t={t} />}
      {hook.handler_type === "prompt" && cfg && <PromptConfigCard cfg={cfg} t={t} />}
    </div>
  );
}

// Narrow Record<string, unknown> values to strings before rendering. JSX
// won't accept `unknown` directly even when the surrounding `&&` proves
// truthiness, so we extract typed locals at the boundary.
type TFn = (key: string) => string;

function HttpConfigCard({ cfg, t }: { cfg: Record<string, unknown>; t: TFn }) {
  const url = typeof cfg.url === "string" ? cfg.url : "";
  const method = typeof cfg.method === "string" ? cfg.method : "";
  const bodyTemplate = typeof cfg.body_template === "string" ? cfg.body_template : "";
  return (
    <div className="rounded-lg border bg-card p-4 space-y-3">
      <p className="text-xs font-medium text-muted-foreground">{t("tabs.config")}</p>
      {url && (
        <div className="space-y-1">
          <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("form.url")}</p>
          <code className="block rounded bg-muted px-2 py-1 text-xs font-mono break-all">{url}</code>
        </div>
      )}
      {method && (
        <div className="space-y-1">
          <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("form.method")}</p>
          <Badge variant="outline" className="font-mono">{method}</Badge>
        </div>
      )}
      {bodyTemplate && (
        <div className="space-y-1">
          <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("form.bodyTemplate")}</p>
          <pre className="overflow-x-auto rounded bg-muted px-2 py-1 text-xs font-mono">{bodyTemplate}</pre>
        </div>
      )}
    </div>
  );
}

function PromptConfigCard({ cfg, t }: { cfg: Record<string, unknown>; t: TFn }) {
  const promptTemplate = typeof cfg.prompt_template === "string" ? cfg.prompt_template : "";
  const model = typeof cfg.model === "string" ? cfg.model : "";
  return (
    <div className="rounded-lg border bg-card p-4 space-y-3">
      <p className="text-xs font-medium text-muted-foreground">{t("tabs.config")}</p>
      {promptTemplate && (
        <div className="space-y-1">
          <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("form.promptTemplate")}</p>
          <pre className="overflow-x-auto rounded bg-muted px-2 py-1 text-xs whitespace-pre-wrap">{promptTemplate}</pre>
        </div>
      )}
      {model && (
        <div className="space-y-1">
          <p className="text-2xs uppercase tracking-wide text-muted-foreground">{t("form.model")}</p>
          <Badge variant="outline">{model}</Badge>
        </div>
      )}
    </div>
  );
}
