import { useEffect, useState, type ReactNode } from "react";
import { Alert, Button, Flex, Spin } from "antd";
import { LogtoProvider, useHandleSignInCallback, useLogto } from "@logto/react";
import { loadMe, setAccessTokenProvider, type PrincipalResponse } from "./api/client";

type AuthConfig = {
  endpoint: string;
  appId: string;
  audience?: string;
};

function readConfig(): AuthConfig | undefined {
  const endpoint = import.meta.env.VITE_LOGTO_ENDPOINT?.trim();
  const appId = import.meta.env.VITE_LOGTO_APP_ID?.trim();
  if (!endpoint || !appId) return undefined;
  return { endpoint, appId, audience: import.meta.env.VITE_LOGTO_AUDIENCE?.trim() || undefined };
}

function SignInCallback() {
  const { isLoading, error } = useHandleSignInCallback(() => { window.history.replaceState({}, document.title, window.location.pathname); });
  if (isLoading) return <div className="loading-screen"><Spin /> Completing sign-in…</div>;
  if (error) return <div className="loading-screen"><Alert type="error" message="Sign-in failed" description={error.message} /></div>;
  return null;
}

function LogtoGate({ children }: { children: ReactNode }) {
  const { isAuthenticated, isLoading, error, signIn, signOut, getAccessToken } = useLogto();
  const [principal, setPrincipal] = useState<PrincipalResponse["principal"] | null>(null);
  const [identityError, setIdentityError] = useState("");
  useEffect(() => {
    setAccessTokenProvider(isAuthenticated ? async () => {
      const token = await getAccessToken();
      return typeof token === "string" ? token : undefined;
    } : undefined);
    return () => setAccessTokenProvider(undefined);
  }, [getAccessToken, isAuthenticated]);
  useEffect(() => {
    if (!isAuthenticated) { setPrincipal(null); setIdentityError(""); return; }
    let cancelled = false;
    loadMe().then((value) => { if (!cancelled) { setPrincipal(value.principal); setIdentityError(""); } })
      .catch((value) => { if (!cancelled) setIdentityError(value instanceof Error ? value.message : "Unable to verify identity"); });
    return () => { cancelled = true; };
  }, [isAuthenticated]);
  if (isLoading) return <div className="loading-screen"><Spin /> Loading authentication…</div>;
  if (!isAuthenticated) return <div className="loading-screen"><Flex vertical gap="middle" align="center"><h2>Sign in to Agora</h2>{error && <Alert type="error" message={error.message} /> }<Button type="primary" onClick={() => void signIn(window.location.origin)}>Sign in</Button></Flex></div>;
  const identity = principal ? `${principal.display_name || principal.user_id}${principal.email ? ` (${principal.email})` : ""}` : "";
  return <>{children}<div className="auth-signout">{identityError ? <span className="auth-identity">Identity check failed: {identityError}</span> : identity && <span className="auth-identity">{identity}</span>}<Button type="link" onClick={() => void signOut(window.location.origin)}>Sign out</Button></div></>;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const config = readConfig();
  if (!config) return <>{children}</>;
  return <LogtoProvider config={{ endpoint: config.endpoint, appId: config.appId, resources: config.audience ? [config.audience] : undefined }}><SignInCallback /><LogtoGate>{children}</LogtoGate></LogtoProvider>;
}
