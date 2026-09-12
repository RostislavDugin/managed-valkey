import { useId, useState } from 'react';
import { ArrowDownFromLine, ArrowUpToLine, Check, Copy } from 'lucide-react';
import { CodeHighlight } from '@mantine/code-highlight';
import { ActionIcon, CopyButton, Group, Tabs, Tooltip, UnstyledButton } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import styles from './ValkeyPage.module.css';

interface ConnectionExamplesProps {
  host: string;
  hostRo: string;
  port: number | string;
}

const LANGUAGES = [
  { value: 'javascript', fileName: 'valkey.js', language: 'javascript' },
  { value: 'typescript', fileName: 'valkey.ts', language: 'typescript' },
  { value: 'python', fileName: 'valkey.py', language: 'python' },
  { value: 'go', fileName: 'valkey.go', language: 'go' },
] as const;

type LanguageValue = (typeof LANGUAGES)[number]['value'];

function getExamples(host: string, hostRo: string, port: number | string) {
  return {
    javascript: `import { createClient } from 'redis';

const primaryHost = '${host}';
const readHost = '${hostRo}';
const createValkeyClient = (host) => createClient({
    username: 'app',
    password: '<PASSWORD>',
    socket: { host, port: ${port}, tls: true, servername: host },
  });

const primary = createValkeyClient(primaryHost);
const readOnly = createValkeyClient(readHost);
primary.on('error', console.error);
readOnly.on('error', console.error);
await Promise.all([primary.connect(), readOnly.connect()]);`,
    typescript: `import { createClient, type RedisClientOptions } from 'redis';

const primaryHost = '${host}';
const readHost = '${hostRo}';
const createValkeyClient = (host: string) => {
  const options = {
    username: 'app',
    password: '<PASSWORD>',
    socket: { host, port: ${port}, tls: true, servername: host },
  } satisfies RedisClientOptions;
  return createClient(options);
};

const primary = createValkeyClient(primaryHost);
const readOnly = createValkeyClient(readHost);
await Promise.all([primary.connect(), readOnly.connect()]);`,
    python: `import redis

primary_host = "${host}"
read_host = "${hostRo}"

def create_client(host):
    return redis.Redis(
        host=host,
        port=${port},
        username="app",
        password="<PASSWORD>",
        ssl=True,
        decode_responses=True,
    )

primary = create_client(primary_host)
read_only = create_client(read_host)
primary.ping()
read_only.ping()`,
    go: `package main

import (
    "context"
    "crypto/tls"
    "log"

    "github.com/redis/go-redis/v9"
)

func main() {
    primary := createClient("${host}")
    readOnly := createClient("${hostRo}")
    defer primary.Close()
    defer readOnly.Close()

    if err := primary.Ping(context.Background()).Err(); err != nil {
        log.Fatal(err)
    }
    if err := readOnly.Ping(context.Background()).Err(); err != nil {
        log.Fatal(err)
    }
}

func createClient(host string) *redis.Client {
    return redis.NewClient(&redis.Options{
        Addr:     host + ":${port}",
        Username: "app",
        Password: "<PASSWORD>",
        TLSConfig: &tls.Config{
            ServerName: host,
            MinVersion: tls.VersionTLS12,
        },
    })
}`,
  };
}

export function ConnectionExamples({ host, hostRo, port }: ConnectionExamplesProps) {
  const codeRegionId = useId();
  const [activeLanguage, setActiveLanguage] = useState<LanguageValue>('javascript');
  const [expanded, setExpanded] = useState(true);
  const examples = getExamples(host, hostRo, port);
  const activeCode = examples[activeLanguage];

  const toggleLabel = expanded ? 'Свернуть' : 'Развернуть';
  const activeRegionId = `${codeRegionId}-${activeLanguage}`;

  return (
    <Tabs
      className={styles.codeCard}
      data-collapsed={!expanded || undefined}
      keepMounted={false}
      onChange={(value) => value && setActiveLanguage(value as LanguageValue)}
      value={activeLanguage}
    >
      <div className={styles.codeHeader}>
        <Tabs.List aria-label="Язык примера подключения">
          {LANGUAGES.map((language) => (
            <Tabs.Tab key={language.value} value={language.value}>
              {language.fileName}
            </Tabs.Tab>
          ))}
        </Tabs.List>

        <Group className={styles.codeActions} gap={2} wrap="nowrap">
          <CopyButton value={activeCode}>
            {({ copied, copy }) => (
              <Tooltip label={copied ? 'Код скопирован' : 'Скопировать код'}>
                <ActionIcon
                  aria-label={copied ? 'Код скопирован' : 'Скопировать код'}
                  onClick={copy}
                  variant={buttonVariants.ghost}
                >
                  {copied ? (
                    <Check aria-hidden="true" size={16} strokeWidth={1.5} />
                  ) : (
                    <Copy aria-hidden="true" size={16} strokeWidth={1.5} />
                  )}
                </ActionIcon>
              </Tooltip>
            )}
          </CopyButton>

          <Tooltip label={toggleLabel}>
            <ActionIcon
              aria-controls={activeRegionId}
              aria-expanded={expanded}
              aria-label={toggleLabel}
              onClick={() => setExpanded((value) => !value)}
              variant={buttonVariants.ghost}
            >
              {expanded ? (
                <ArrowUpToLine aria-hidden="true" size={16} strokeWidth={1.5} />
              ) : (
                <ArrowDownFromLine aria-hidden="true" size={16} strokeWidth={1.5} />
              )}
            </ActionIcon>
          </Tooltip>
        </Group>
      </div>

      {LANGUAGES.map((language) => (
        <Tabs.Panel key={language.value} value={language.value}>
          <div className={styles.codeViewport} id={`${codeRegionId}-${language.value}`}>
            <CodeHighlight
              background="h3_bg_2"
              classNames={{ code: styles.codePre, codeHighlight: styles.codeHighlight }}
              code={examples[language.value]}
              language={language.language}
              withCopyButton={false}
            />

            {!expanded && (
              <UnstyledButton
                aria-label="Развернуть код"
                className={styles.codeExpandButton}
                onClick={() => setExpanded(true)}
              >
                Развернуть
              </UnstyledButton>
            )}
          </div>
        </Tabs.Panel>
      ))}
    </Tabs>
  );
}
