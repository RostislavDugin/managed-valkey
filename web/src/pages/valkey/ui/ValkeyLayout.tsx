import { createContext, use, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Link, Outlet, useLocation, useOutletContext } from 'react-router';
import { Anchor, Breadcrumbs, Text } from '@mantine/core';
import type { Session } from '@/shared/api';
import { routes, valkeyInstancePath } from '@/shared/config';
import type { EphemeralValkeyPassword } from '../model/valkey-credentials';
import styles from './ValkeyLayout.module.css';

/** Каркас консоли отдаёт разделу узлы шапки и правой колонки. */
interface ConsoleOutletContext {
  session: Session;
  headerSlot: HTMLElement | null;
  asideSlot: HTMLElement | null;
}

interface ValkeyContextValue {
  session: Session;
  asideSlot: HTMLElement | null;
  headerSlot: HTMLElement | null;
  setTrailingCrumb: (crumb: string | null) => void;
  ephemeralPassword: EphemeralValkeyPassword | null;
  setEphemeralPassword: (value: EphemeralValkeyPassword) => void;
  clearEphemeralPassword: () => void;
}

const ValkeyContext = createContext<ValkeyContextValue | null>(null);

export function useValkeySection() {
  const value = use(ValkeyContext);
  if (!value) {
    throw new Error('useValkeySection доступен только внутри ValkeyLayout');
  }
  return value;
}

export function ValkeyLayout() {
  const { asideSlot, headerSlot, session } = useOutletContext<ConsoleOutletContext>();
  const { pathname } = useLocation();
  const [trailingCrumb, setTrailingCrumb] = useState<string | null>(null);
  const [ephemeralPassword, setEphemeralPassword] = useState<EphemeralValkeyPassword | null>(null);
  const previousPathnameRef = useRef(pathname);

  const clearEphemeralPassword = useCallback(() => setEphemeralPassword(null), []);

  useEffect(() => {
    window.addEventListener('pagehide', clearEphemeralPassword);
    return () => window.removeEventListener('pagehide', clearEphemeralPassword);
  }, [clearEphemeralPassword]);

  useEffect(() => {
    const pathnameChanged = previousPathnameRef.current !== pathname;
    previousPathnameRef.current = pathname;

    if (
      pathnameChanged &&
      ephemeralPassword &&
      pathname !== valkeyInstancePath(ephemeralPassword.instanceId)
    ) {
      clearEphemeralPassword();
    }
  }, [clearEphemeralPassword, ephemeralPassword, pathname]);

  const context = useMemo<ValkeyContextValue>(
    () => ({
      session,
      asideSlot,
      headerSlot,
      setTrailingCrumb,
      ephemeralPassword,
      setEphemeralPassword,
      clearEphemeralPassword,
    }),
    [asideSlot, clearEphemeralPassword, ephemeralPassword, headerSlot, session]
  );

  const crumbs = [
    { label: 'Базы данных', to: routes.valkeyManagement },
    ...(trailingCrumb ? [{ label: 'Valkey', to: routes.valkeyManagement }] : []),
    ...(trailingCrumb ? [{ label: trailingCrumb, to: undefined }] : []),
  ];

  const breadcrumbs = (
    <Breadcrumbs
      className={styles.breadcrumbs}
      component="nav"
      aria-label="Хлебные крошки"
      separator="/"
      separatorMargin="h3_xs"
    >
      {crumbs.map((crumb, index) =>
        index === crumbs.length - 1 || !crumb.to ? (
          <Text
            key={crumb.label}
            className={styles.crumb}
            c="h3_text"
            fw="var(--h3-fw-medium-1)"
            size="h3_sm"
          >
            {crumb.label}
          </Text>
        ) : (
          <Anchor
            key={crumb.label}
            className={styles.crumb}
            component={Link}
            c="h3_text"
            fw="var(--h3-fw-medium-1)"
            size="h3_sm"
            to={crumb.to}
            underline="hover"
          >
            {crumb.label}
          </Anchor>
        )
      )}
    </Breadcrumbs>
  );

  return (
    <>
      {headerSlot ? createPortal(breadcrumbs, headerSlot) : null}

      <ValkeyContext value={context}>
        <Outlet />
      </ValkeyContext>
    </>
  );
}
