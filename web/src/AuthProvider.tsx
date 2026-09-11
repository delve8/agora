import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { Alert, Button, Flex, Space, Spin } from "antd";
import { LogtoProvider, useHandleSignInCallback, useLogto, type IdTokenClaims } from "@logto/react";
import { loadMe, setAccessTokenProvider, type PrincipalResponse } from "./api/client";

// AuthActions lets the app shell render the signed-in identity and sign-out
// control where it belongs (the header) instead of an overlay pinned to a
// corner of the viewport.
type AuthActions = { identity: string; signOut: () => void };

const AuthActionsContext = createContext<AuthActions | null>(null);

export function useAuthActions() {
  return useContext(AuthActionsContext);
}

type AuthConfig = {
  endpoint: string;
  appId: string;
  audience?: string;
};

type ConfigState =
  | { status: "loading" }
  | { status: "none" }
  | { status: "incomplete"; reason: string }
  | { status: "ready"; config: AuthConfig };

// Build-time settings remain supported for `vite dev` and for bundles built by
// hand. A published image carries none: Vite would have inlined them into the
// JavaScript, which is exactly why the Server serves them from /api/config
// instead, so one image can serve any tenant.
function configFromEnv(): AuthConfig | undefined {
  const endpoint = import.meta.env.VITE_LOGTO_ENDPOINT?.trim();
  const appId = import.meta.env.VITE_LOGTO_APP_ID?.trim();
  if (!endpoint || !appId) return undefined;
  return { endpoint, appId, audience: import.meta.env.VITE_LOGTO_AUDIENCE?.trim() || undefined };
}

function stringField(source: Record<string, unknown>, key: string): string {
  const value = source[key];
  return typeof value === "string" ? value.trim() : "";
}

async function loadConfig(): Promise<ConfigState> {
  const fromEnv = configFromEnv();
  if (fromEnv) return { status: "ready", config: fromEnv };
  let body: Record<string, unknown>;
  try {
    const response = await fetch("/api/config", { headers: { accept: "application/json" } });
    if (!response.ok) return { status: "none" };
    body = (await response.json()) as Record<string, unknown>;
  } catch {
    // The Server is unreachable. Render the app anyway: /api/me reports the
    // failure and the sign-in screen explains it.
    return { status: "none" };
  }
  const mode = stringField(body, "auth_mode");
  if (mode && mode !== "logto") return { status: "none" };
  const endpoint = stringField(body, "logto_endpoint");
  const appId = stringField(body, "logto_app_id");
  const audience = stringField(body, "logto_audience");
  if (!endpoint || !appId) {
    if (mode === "logto") {
      return {
        status: "incomplete",
        reason: "Set AGORA_LOGTO_APP_ID, and AGORA_LOGTO_ENDPOINT unless the issuer already ends in /oidc.",
      };
    }
    return { status: "none" };
  }
  return { status: "ready", config: { endpoint, appId, audience: audience || undefined } };
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

// The API validates Logto's access token, and an access token carries no profile
// claims: username, name and email exist only in the ID token (or userinfo). The
// signed-in label therefore comes from the browser's ID token, with the Server's
// principal as the fallback for providers whose access token does carry claims.
function preferredUserLabel(claims?: IdTokenClaims | null): string {
  const record = claims as (Record<string, unknown> | null | undefined);
  for (const value of [claims?.username, record?.["preferred_username"], claims?.name, claims?.email]) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function LogtoGate({ children, audience }: { children: ReactNode; audience?: string }) {
  const { isAuthenticated, isLoading, error, signIn, signOut, getAccessToken, getIdTokenClaims, clearAccessToken, clearAllTokens } = useLogto();
  const [principal, setPrincipal] = useState<PrincipalResponse["principal"] | null>(null);
  const [identityError, setIdentityError] = useState("");
  const [identityLoading, setIdentityLoading] = useState(true);
  const [idTokenName, setIdTokenName] = useState("");
  const [idTokenEmail, setIdTokenEmail] = useState("");
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
  useEffect(() => {
    if (!isAuthenticated) {
      setIdTokenName("");
      setIdTokenEmail("");
      return;
    }
    let cancelled = false;
    void getIdTokenClaims()
      .then((claims) => {
        if (cancelled || !claims) return;
        setIdTokenName(preferredUserLabel(claims));
        setIdTokenEmail(typeof claims.email === "string" ? claims.email.trim() : "");
      })
      .catch(() => {
        // The principal from /api/me stays the fallback label.
      });
    return () => { cancelled = true; };
  }, [isAuthenticated, getIdTokenClaims]);

  // Logto marks the provider as loading while refreshing an access token. Keep
  // authenticated children mounted during that request; otherwise a device
  // action can make the Drawer disappear while its API call is authenticating.
  if (isLoading && !isAuthenticated) return <div className="loading-screen"><Spin /> Loading authentication…</div>;
  if (!isAuthenticated) return <div className="loading-screen"><Flex vertical gap="middle" align="center"><h2>Sign in to Agora</h2>{error && <Alert type="error" message={error.message} /> }<Button type="primary" onClick={() => void signIn(authRedirectUri())}>Sign in</Button></Flex></div>;
  if (identityLoading || !principal) return <div className="loading-screen"><Flex vertical gap="middle" align="center"><Spin />{identityError ? <Alert type="error" message="Agora authentication failed" description={identityError} /> : <span>Verifying identity…</span>}<Space>{identityError && <Button onClick={() => void signIn(authRedirectUri())}>Sign in again</Button>}<Button type="link" onClick={() => void signOut(authRedirectUri())}>Sign out</Button></Space></Flex></div>;
  const label = idTokenName || principal.display_name || principal.user_id;
  const email = principal.email || idTokenEmail;
  const identity = email && !label.includes(email) ? `${label} (${email})` : label;
  const actions: AuthActions = { identity, signOut: () => void signOut(authRedirectUri()) };
  return <AuthActionsContext.Provider value={actions}>{children}</AuthActionsContext.Provider>;
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
  const [state, setState] = useState<ConfigState>({ status: "loading" });
  useEffect(() => {
    let cancelled = false;
    void loadConfig().then((value) => { if (!cancelled) setState(value); });
    return () => { cancelled = true; };
  }, []);
  if (state.status === "loading") return <div className="loading-screen"><Spin /> Loading configuration…</div>;
  if (state.status === "incomplete") {
    return <div className="loading-screen"><Alert type="error" showIcon message="Agora requires sign-in, but the Web UI has no Logto application id" description={state.reason} /></div>;
  }
  if (state.status === "none") return <>{children}</>;
  const config = state.config;
  return <LogtoProvider config={{ endpoint: config.endpoint, appId: config.appId, resources: config.audience ? [config.audience] : undefined }}><LogtoContent audience={config.audience}>{children}</LogtoContent></LogtoProvider>;
}
