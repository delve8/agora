import { useCallback, useEffect, useState } from "react";
import { listDevices } from "../api/client";
import type { Device } from "../types";

// useDevices polls the current user's paired devices so the device list, the
// session badges and the create-session picker all stay fresh. It is
// best-effort: failures are swallowed and the next 5s tick retries, because
// the device list is secondary to the coordination/session state.
export function useDevices() {
  const [devices, setDevices] = useState<Device[]>([]);

  const refresh = useCallback(async () => {
    try {
      setDevices(await listDevices());
    } catch {
      /* keep the last known device list */
    }
  }, []);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), 5000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  // deviceName resolves a session's daemon_id to its friendly alias. Unknown
  // daemons (e.g. unpaired local-mode connections) resolve to "" so the UI can
  // skip the badge instead of showing a raw uuid.
  const deviceName = useCallback(
    (daemonID?: string) => (daemonID ? devices.find((device) => device.device_id === daemonID)?.name || "" : ""),
    [devices],
  );

  return { devices, refresh, deviceName };
}
