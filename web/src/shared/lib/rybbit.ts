const RYBBIT_SCRIPT_SELECTOR =
  'script[src="https://rybbit.databasus.com/api/script.js"][data-site-id]';
const RYBBIT_WAIT_INTERVAL_MS = 100;
const RYBBIT_WAIT_TIMEOUT_MS = 10_000;

interface RybbitClient {
  clearUserId(): void;
  identify(userId: string, traits: { email: string }): void;
}

export interface RybbitUser {
  email: string;
  userId: string;
}

declare global {
  interface Window {
    rybbit?: RybbitClient;
  }
}

let synchronizationRevision = 0;

function applyRybbitUser(user: RybbitUser | null) {
  if (!window.rybbit) {
    return false;
  }

  try {
    if (user) {
      window.rybbit.identify(user.userId, { email: user.email });
    } else {
      window.rybbit.clearUserId();
    }
  } catch {
    return true;
  }

  return true;
}

function waitForRybbit(user: RybbitUser | null, revision: number, startedAt: number) {
  if (revision !== synchronizationRevision || applyRybbitUser(user)) {
    return;
  }

  if (Date.now() - startedAt >= RYBBIT_WAIT_TIMEOUT_MS) {
    return;
  }

  window.setTimeout(() => waitForRybbit(user, revision, startedAt), RYBBIT_WAIT_INTERVAL_MS);
}

export function setRybbitUser(user: RybbitUser | null) {
  const revision = ++synchronizationRevision;

  if (!document.querySelector(RYBBIT_SCRIPT_SELECTOR)) {
    return;
  }

  waitForRybbit(user, revision, Date.now());
}
