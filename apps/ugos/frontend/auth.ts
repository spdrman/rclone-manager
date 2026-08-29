interface UgosSession {
  username: string;
  expiresAt: string;
}

interface UgosHost {
  getSession?(): Promise<{ user: string; expires: string } | null>;
}

/** The ONLY file in the repo that talks to a UGOS-provided global. If UGOS is
 *  not present (dev, or a mis-packaged build) we return null and the shared UI
 *  falls back to its own login rather than hanging on a missing host. */
export async function getUgosSession(): Promise<UgosSession | null> {
  const host = (window as unknown as { ugos?: UgosHost }).ugos;
  if (!host?.getSession) return null;

  try {
    const session = await host.getSession();
    if (!session) return null;
    return { username: session.user, expiresAt: session.expires };
  } catch {
    return null;
  }
}
