import { useId, useState } from 'react';
import { ArrowDownFromLine, ArrowUpToLine, Check, Copy } from 'lucide-react';
import { CodeHighlight } from '@mantine/code-highlight';
import { ActionIcon, CopyButton, Group, Tabs, Tooltip, UnstyledButton } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import styles from './ValkeyPage.module.css';

interface ConnectionExamplesProps {
  host: string;
  port: number | string;
}

const LANGUAGES = [
  { value: 'javascript', fileName: 'valkey.js', language: 'javascript' },
  { value: 'typescript', fileName: 'valkey.ts', language: 'typescript' },
  { value: 'python', fileName: 'valkey.py', language: 'python' },
  { value: 'go', fileName: 'valkey.go', language: 'go' },
] as const;

type LanguageValue = (typeof LANGUAGES)[number]['value'];

function getExamples(host: string, port: number | string) {
  return {
    javascript: `import { createClient } from 'redis';

const host = '${host}';
const client = createClient({
  username: 'app',
  password: '<PASSWORD>',
  socket: {
    host,
    port: ${port},
    tls: true,
    servername: host,
  },
});

client.on('error', console.error);
await client.connect();`,
    typescript: `import { createClient, type RedisClientOptions } from 'redis';

const host = '${host}';
const options = {
  username: 'app',
  password: '<PASSWORD>',
  socket: {
    host,
    port: ${port},
    tls: true,
    servername: host,
  },
} satisfies RedisClientOptions;

const client = createClient(options);
client.on('error', console.error);
await client.connect();`,
    python: `import redis

client = redis.Redis(
    host="${host}",
    port=${port},
    username="app",
    password="<PASSWORD>",
    ssl=True,
    decode_responses=True,
)

client.ping()`,
    go: `package main

import (
    "context"
    "crypto/tls"
    "log"

    "github.com/redis/go-redis/v9"
)

func main() {
    const serverName = "${host}"
    client := redis.NewClient(&redis.Options{
        Addr:     "${host}:${port}",
        Username: "app",
        Password: "<PASSWORD>",
        TLSConfig: &tls.Config{
            ServerName: serverName,
            MinVersion: tls.VersionTLS12,
        },
    })
    defer client.Close()

    if err := client.Ping(context.Background()).Err(); err != nil {
        log.Fatal(err)
    }
}`,
  };
}

export function ConnectionExamples({ host, port }: ConnectionExamplesProps) {
  const codeRegionId = useId();
  const [activeLanguage, setActiveLanguage] = useState<LanguageValue>('javascript');
  const [expanded, setExpanded] = useState(true);
  const examples = getExamples(host, port);
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
