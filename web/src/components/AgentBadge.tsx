import { RobotOutlined } from "@ant-design/icons";
import { Tag, Tooltip } from "antd";
import type { ReactNode } from "react";

export function agentLabel(agent?: string) {
  switch ((agent || "").trim().toLowerCase()) {
    case "claude":
    case "claude-code":
      return "Claude Code";
    case "pi":
      return "Pi";
    default:
      return agent?.trim() || "未知 Agent";
  }
}

function agentColor(agent?: string) {
  switch ((agent || "").trim().toLowerCase()) {
    case "claude":
    case "claude-code":
      return "volcano";
    case "pi":
      return "geekblue";
    default:
      return "default";
  }
}

function ClaudeLogo() {
  return <svg className="agent-logo agent-logo-claude" viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="currentColor">
    <path d="M13.04 2.25h-2.08l-.35 6.18-2.99-5.42-1.8 1.04 2.79 5.07-5.18-3.09-1.04 1.8 5.36 3.2H1.5v2.08h6.23l-5.34 3.08 1.04 1.8 5.18-2.99-2.79 5.07 1.8 1.04 2.99-5.42.35 6.06h2.08l.35-6.06 2.99 5.42 1.8-1.04-2.79-5.07 5.18 2.99 1.04-1.8-5.34-3.08h6.23v-2.08h-6.25l5.36-3.2-1.04-1.8-5.18 3.09 2.79-5.07-1.8-1.04-2.99 5.42-.35-6.18Z" />
  </svg>;
}

function PiLogo() {
  return <svg className="agent-logo agent-logo-pi" viewBox="0 0 800 800" aria-hidden="true" focusable="false" fill="currentColor">
    <path fill="#09090b" fillRule="evenodd" d="M165.29 165.29 H517.36 V400 H400 V517.36 H282.65 V634.72 H165.29 Z M282.65 282.65 V400 H400 V282.65 Z" />
    <path fill="#09090b" d="M517.36 400 H634.72 V634.72 H517.36 Z" />
  </svg>;
}

function agentIcon(agent?: string): ReactNode {
  switch ((agent || "").trim().toLowerCase()) {
    case "claude":
    case "claude-code":
      return <ClaudeLogo />;
    case "pi":
      return <PiLogo />;
    default:
      return <RobotOutlined />;
  }
}

/** A compact provider marker. The tooltip keeps the full name available
 * without taking room in session selectors and headers. */
export function AgentBadge({ agent, compact = false, iconOnly = false, className }: { agent?: string; compact?: boolean; iconOnly?: boolean; className?: string }) {
  const label = agentLabel(agent);
  if (iconOnly) {
    return <Tooltip title={label}><span className="agent-badge-icon-only" aria-label={label}>{agentIcon(agent)}</span></Tooltip>;
  }
  return <Tooltip title={label}>
    <Tag className={`agent-badge${compact ? " agent-badge-compact" : ""}${className ? ` ${className}` : ""}`} color={agentColor(agent)} aria-label={label} icon={agentIcon(agent)} />
  </Tooltip>;
}
