import { useEffect, useRef, useState, type ReactNode } from "react";
import { Alert, Button, Flex, Space, Spin } from "antd";
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

function SignInCallback({ onComplete }: { onComplete: () => void }) {
  const { isLoading, error } = useHandleSignInCallback(() => {
    window.history.replaceState({}, document.title, window.location.pathname);
    onComplete();
  });
  if (isLoading) return <div className="loading-screen"><Spin /> Completing sign-in…</div>;
  if (error) return <div className="loading-screen"><Alert type="error" message="Sign-in failed" description={error.message} /></div>;
  return null;
}

function isAuthCallbackUri(): boolean {
  const params = new URLSearchParams(window.location.search);
  return params.has("code") || params.has("state") || params.has("error");
}

function authRedirectUri(): string {
  return new URL("/", window.location.origin).toString();
}

function isJwt(value: string): boolean {
  const parts = value.trim().split(".");
  return parts.length === 3 && parts.every((part) => part.length > 0);
}

function isInvalidTokenError(value: unknown): boolean {
  return value instanceof Error && /invalid JWT format|invalid token/i.test(value.message);
}

function LogtoGate({ children, audience }: { children: ReactNode; audience?: string }) {
  const { isAuthenticated, isLoading, error, signIn, signOut, getAccessToken, clearAccessToken, clearAllTokens } = useLogto();
  const [principal, setPrincipal] = useState<PrincipalResponse["principal"] | null>(null);
  const [identityError, setIdentityError] = useState("");
  const [identityLoading, setIdentityLoading] = useState(true);
  const invalidation = useRef<Promise<void> | null>(null);
  useEffect(() => {
    const invalidate = async () => {
      // Several polling requests can receive the same stale token at once.
      // Serialize cache invalidation so one request cannot clear the cache
      // while another request is already fetching the replacement token.
      if (!invalidation.current) {
        invalidation.current = clearAccessToken().finally(() => { invalidation.current = null; });
      }
      await invalidation.current;
    };
    setAccessTokenProvider(isAuthenticated ? async () => {
      let token = await getAccessToken(audience);
      if (audience && typeof token === "string" && !isJwt(token)) {
        // An old/default-resource access token can be cached by Logto after a
        // previous build. Never send an opaque token to Agora's JWT validator.
        await invalidate();
        token = await getAccessToken(audience);
      }
      if (typeof token !== "string" || (audience && !isJwt(token))) {
        throw new Error("Logto did not return a JWT for the Agora API resource");
      }
      return token;
    } : undefined, isAuthenticated ? invalidate : undefined);
    return () => setAccessTokenProvider(undefined);
  }, [clearAccessToken, getAccessToken, isAuthenticated, audience]);
  useEffect(() => {
    if (!isAuthenticated) { setPrincipal(null); setIdentityError(""); setIdentityLoading(false); return; }
    let cancelled = false;
    setIdentityLoading(true);
    void (async () => {
      try {
        // The authorization-code callback may have cached the default-resource
        // token. Clear only the access-token cache before requesting the
        // explicitly configured Agora API resource.
        await clearAccessToken();
        const value = await loadMe();
        if (!cancelled) { setPrincipal(value.principal); setIdentityError(""); setIdentityLoading(false); }
      } catch (value) {
        if (cancelled) return;
        if (isInvalidTokenError(value)) {
          // Do not mount the application with a broken bearer token: its
          // polling requests would otherwise repeat the same 401 forever.
          await clearAllTokens();
        }
        setIdentityError(value instanceof Error ? value.message : "Unable to verify identity");
        setIdentityLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [clearAccessToken, clearAllTokens, isAuthenticated, audience]);
  // Logto marks the provider as loading while refreshing an access token. Keep
  // authenticated children mounted during that request; otherwise a device
  // action can make the Drawer disappear while its API call is authenticating.
  if (isLoading && !isAuthenticated) return <div className="loading-screen"><Spin /> Loading authentication…</div>;
  if (!isAuthenticated) return <div className="loading-screen"><Flex vertical gap="middle" align="center"><h2>Sign in to Agora</h2>{error && <Alert type="error" message={error.message} /> }<Button type="primary" onClick={() => void signIn(authRedirectUri())}>Sign in</Button></Flex></div>;
  if (identityLoading || !principal) return <div className="loading-screen"><Flex vertical gap="middle" align="center"><Spin />{identityError ? <Alert type="error" message="Agora authentication failed" description={identityError} /> : <span>Verifying identity…</span>}<Space>{identityError && <Button onClick={() => void signIn(authRedirectUri())}>Sign in again</Button>}<Button type="link" onClick={() => void signOut(authRedirectUri())}>Sign out</Button></Space></Flex></div>;
  const identity = `${principal.display_name || principal.user_id}${principal.email ? ` (${principal.email})` : ""}`;
  return <>{children}<div className="auth-signout"><span className="auth-identity">{identity}</span><Button type="link" onClick={() => void signOut(authRedirectUri())}>Sign out</Button></div></>;
}

function LogtoContent({ children, audience }: { children: ReactNode; audience?: string }) {
  const callback = isAuthCallbackUri();
  const [callbackComplete, setCallbackComplete] = useState(!callback);
  return <>
    {callback && <SignInCallback onComplete={() => setCallbackComplete(true)} />}
    {callbackComplete ? <LogtoGate audience={audience}>{children}</LogtoGate> : <div className="loading-screen"><Spin /> Completing sign-in…</div>}
  </>;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const config = readConfig();
  if (!config) return <>{children}</>;
  return <LogtoProvider config={{ endpoint: config.endpoint, appId: config.appId, resources: config.audience ? [config.audience] : undefined }}><LogtoContent audience={config.audience}>{children}</LogtoContent></LogtoProvider>;
}
