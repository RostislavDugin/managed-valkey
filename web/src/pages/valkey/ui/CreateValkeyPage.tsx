import { useCallback, useEffect, useState } from 'react';
import { RefreshCw, RotateCw } from 'lucide-react';
import { useNavigate } from 'react-router';
import {
  ActionIcon,
  Alert,
  Anchor,
  Button,
  Container,
  Group,
  Radio,
  SimpleGrid,
  Skeleton,
  Stack,
  Switch,
  Text,
  TextInput,
  Textarea,
  Title,
} from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants, valkeyInstancePath } from '@/shared/config';
import { getValkeyDomain, type ValkeyDomain } from '../api/valkey-config';
import { createInstance, listInstances } from '../api/valkey-storage';
import {
  findLargestAvailableSize,
  USER_QUOTA,
  type QuotaAmount,
  type QuotaCheck,
} from '../model/quota';
import {
  formatRam,
  formatVcpu,
  formatInstanceHost,
  generateInstanceName,
  getTotalResources,
  getSlugPreview,
  INSTANCE_PREFIX_MAX_LENGTH,
  validateInstanceName,
  validateInstancePrefix,
  parseWhitelistCidrs,
  validateWhitelistCidrs,
  type PricePeriod,
  type ValkeyInstance,
  type ValkeyMode,
  type ValkeySize,
} from '../model/valkey';
import { generateValkeyPassword } from '../model/valkey-credentials';
import {
  checkCandidateQuota,
  getCreateFormDefaults,
  getRequestErrorMessage,
  SUPPORT_URL,
  type CreateFormValues,
} from '../model/valkey-form';
import { FormRow } from './FormRow';
import { PricePanel, PricePeriodTabs } from './PricePanel';
import { SizePlans } from './SizePlans';
import { ValkeyAside } from './ValkeyAside';
import { useValkeySection } from './ValkeyLayout';
import styles from './ValkeyPage.module.css';

const MODE_CARDS: Array<{ description: string; label: string; value: ValkeyMode }> = [
  {
    value: 'single',
    label: 'Одна нода',
    description:
      'Для разработки, тестов и небольшой нагрузки. Не рекомендуется для production-систем',
  },
  {
    value: 'ha',
    label: 'Отказоустойчивый',
    description: 'Одна primary и две реплики. Для высокой нагрузки и доступности',
  },
];

function formatResources(resources: QuotaAmount) {
  return `${formatVcpu(resources.vcpu)} и ${formatRam(resources.ramGb)} RAM`;
}

function formatMissingResources(quota: QuotaCheck) {
  const missing: string[] = [];

  if (quota.missing.vcpu > 0) {
    missing.push(formatVcpu(quota.missing.vcpu));
  }

  if (quota.missing.ramGb > 0) {
    missing.push(`${formatRam(quota.missing.ramGb)} RAM`);
  }

  return missing.join(' и ');
}

function MaximumConfiguration({ mode, size }: { mode: ValkeyMode; size: ValkeySize | null }) {
  if (!size) {
    return (
      <Text size="h3_sm">
        {mode === 'single' ? 'Одна нода' : 'Отказоустойчивый режим'}: свободной квоты не хватает
        даже на минимальную конфигурацию.
      </Text>
    );
  }

  if (mode === 'single') {
    return <Text size="h3_sm">Одна нода: {formatResources(size)}.</Text>;
  }

  const total = getTotalResources(size, mode);
  return (
    <Text size="h3_sm">
      Отказоустойчивый режим: {formatResources(size)} на каждой ноде, всего {formatResources(total)}
      .
    </Text>
  );
}

interface SupportNoteProps {
  haMaximum: ValkeySize | null;
  quota: QuotaCheck;
  singleMaximum: ValkeySize | null;
}

function SupportNote({ haMaximum, quota, singleMaximum }: SupportNoteProps) {
  return (
    <Alert className={styles.quotaAlert} color="red" title="Недостаточно квоты" variant="light">
      <Stack align="flex-start" gap="h3_sm">
        <Stack gap="h3_xs">
          <Text size="h3_sm">Ваша квота: {formatResources(USER_QUOTA)}.</Text>
          <Text size="h3_sm">Свободно сейчас: {formatResources(quota.available)}.</Text>
          <Text size="h3_sm">
            Для выбранной конфигурации не хватает: {formatMissingResources(quota)}.
          </Text>
        </Stack>

        <Stack gap="h3_xs">
          <Text fw="var(--h3-fw-medium)" size="h3_sm">
            Максимально доступная конфигурация сейчас
          </Text>
          <MaximumConfiguration mode="single" size={singleMaximum} />
          <MaximumConfiguration mode="ha" size={haMaximum} />
        </Stack>

        <Anchor href={SUPPORT_URL} rel="noreferrer" size="h3_sm" target="_blank">
          Напишите в поддержку для увеличения квоты
        </Anchor>
      </Stack>
    </Alert>
  );
}

export function CreateValkeyPage() {
  const { session, setEphemeralPassword, setTrailingCrumb } = useValkeySection();
  const navigate = useNavigate();

  const [instances, setInstances] = useState<ValkeyInstance[] | null>(null);
  const [domain, setDomain] = useState<ValkeyDomain | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [period, setPeriod] = useState<PricePeriod>('month');
  const [submitting, setSubmitting] = useState(false);

  const form = useForm<CreateFormValues>({
    mode: 'controlled',
    initialValues: {
      name: '',
      prefix: '',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
      isWhitelistEnabled: false,
      whitelist: '',
    },
    validateInputOnBlur: true,
    validate: {
      name: (value) => validateInstanceName(value.trim()),
      prefix: (value) => validateInstancePrefix(value.trim()),
      whitelist: (value, formValues) =>
        formValues.isWhitelistEnabled ? validateWhitelistCidrs(value) : null,
    },
  });

  const { setValues } = form;

  const load = useCallback(async () => {
    setLoadError(null);
    setInstances(null);

    try {
      // Недоступный домен не мешает создать базу, поэтому подсказка обходится
      // без него, а форма открывается как обычно.
      const [loaded, loadedDomain] = await Promise.all([
        listInstances(session.userId),
        getValkeyDomain().catch(() => null),
      ]);

      setInstances(loaded);
      setDomain(loadedDomain);
      setValues(getCreateFormDefaults(loaded));
    } catch (error) {
      setLoadError(getRequestErrorMessage(error));
    }
  }, [session.userId, setValues]);

  useEffect(() => {
    setTrailingCrumb('Создание БД');
    return () => setTrailingCrumb(null);
  }, [setTrailingCrumb]);

  useEffect(() => {
    void load();
  }, [load]);

  const values = form.getValues();
  const size = { vcpu: values.vcpu, ramGb: values.ramGb };
  const quota = checkCandidateQuota(instances ?? [], { size, mode: values.mode });
  const singleMaximum = findLargestAvailableSize(instances ?? [], 'single');
  const haMaximum = findLargestAvailableSize(instances ?? [], 'ha');

  const slugPreview = getSlugPreview(values.prefix.trim() || 'valkey');
  const addressPreview = domain ? formatInstanceHost(slugPreview, domain.domain) : slugPreview;

  const submit = async (formValues: CreateFormValues) => {
    setSubmitting(true);
    const password = generateValkeyPassword();

    try {
      const created = await createInstance(session, {
        name: formValues.name.trim(),
        prefix: formValues.prefix.trim(),
        mode: formValues.mode,
        vcpu: formValues.vcpu,
        ramGb: formValues.ramGb,
        password,
        isWhitelistEnabled: formValues.isWhitelistEnabled,
        whitelistCidrs: formValues.isWhitelistEnabled
          ? parseWhitelistCidrs(formValues.whitelist)
          : [],
      });

      notifications.show({ message: `База ${created.name} готова.`, title: 'База создана' });
      await navigate(valkeyInstancePath(created.id));
      setEphemeralPassword({ instanceId: created.id, password, source: 'creation' });
    } catch (error) {
      if (
        error instanceof ApiError &&
        (error.code === 'CONFLICT' || error.code === 'VALIDATION_FAILED')
      ) {
        form.setFieldError('name', error.message);
      } else {
        notifications.show({
          color: 'red',
          message: getRequestErrorMessage(error),
          title: 'Не удалось создать базу',
        });
      }
    } finally {
      setSubmitting(false);
    }
  };

  if (loadError) {
    return (
      <Container className={styles.page} size="h3_page">
        <Alert color="red" title="Не удалось открыть форму">
          <Stack align="flex-start" gap="h3_sm">
            <Text size="h3_sm">{loadError}</Text>
            <Button
              leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
              onClick={() => void load()}
              variant={buttonVariants.secondary}
            >
              Повторить
            </Button>
          </Stack>
        </Alert>
      </Container>
    );
  }

  if (!instances) {
    return (
      <Container className={styles.page} size="h3_page">
        <Stack gap="h3_md">
          <Skeleton height={30} width={220} />
          <Skeleton height={92} />
          <Skeleton height={64} />
          <Skeleton height={64} />
          <Skeleton height={36} />
        </Stack>
      </Container>
    );
  }

  return (
    <Container className={styles.page} size="h3_page">
      <ValkeyAside
        header={<PricePeriodTabs onChange={setPeriod} period={period} />}
        label="Стоимость"
      >
        <PricePanel mode={values.mode} period={period} size={size} />
      </ValkeyAside>

      <Title className={styles.pageTitle} order={1}>
        Новая Valkey база
      </Title>

      <form onSubmit={form.onSubmit((formValues) => void submit(formValues))}>
        <Stack gap="h3_lg">
          <FormRow
            hint="В режиме «Одна нода» база работает на одном сервере. В отказоустойчивом режиме primary и две реплики размещаются на разных физических серверах. Если primary недоступен из-за сбоя или проблем с сетью, одна из реплик автоматически становится primary."
            label="Режим"
            fullWidth
          >
            <Radio.Group
              {...form.getInputProps('mode')}
              onChange={(value) => form.setFieldValue('mode', value as ValkeyMode)}
            >
              <SimpleGrid cols={{ base: 1, mobile: 2 }} spacing={10}>
                {MODE_CARDS.map((card) => (
                  <Radio.Card
                    key={card.value}
                    className={styles.modeCard}
                    p={10}
                    radius="h3_md"
                    value={card.value}
                  >
                    <Group align="flex-start" gap={10} wrap="nowrap">
                      <Radio.Indicator
                        aria-label={card.label}
                        color="h3_bg_accent"
                        iconColor="h3_mint.8"
                      />

                      <div>
                        <Text fw="var(--h3-fw-medium)" size="h3_sm">
                          {card.label}
                        </Text>
                        <Text c="h3_text_2" size="h3_xs">
                          {card.description}
                        </Text>
                      </div>
                    </Group>
                  </Radio.Card>
                ))}
              </SimpleGrid>
            </Radio.Group>
          </FormRow>

          <SizePlans
            isAvailable={(plan) =>
              checkCandidateQuota(instances, { size: plan, mode: values.mode }).fits
            }
            mode={values.mode}
            onChange={(next) => setValues({ vcpu: next.vcpu, ramGb: next.ramGb })}
            size={size}
          />

          {!quota.fits && (
            <SupportNote haMaximum={haMaximum} quota={quota} singleMaximum={singleMaximum} />
          )}

          <FormRow
            hint="Имя отображается только в консоли. Его можно изменить после создания."
            htmlFor="valkey-name"
            label="Имя"
          >
            <Group align="flex-start" gap="h3_xs" wrap="nowrap">
              <TextInput
                id="valkey-name"
                placeholder="valkey-1474"
                style={{ flex: 1 }}
                {...form.getInputProps('name')}
              />

              <ActionIcon
                aria-label="Сгенерировать другое имя"
                className={styles.touchTarget}
                onClick={() =>
                  form.setFieldValue(
                    'name',
                    generateInstanceName(instances.map((instance) => instance.name))
                  )
                }
                size="input-md"
                variant={buttonVariants.stroke}
              >
                <RefreshCw aria-hidden="true" size={16} strokeWidth={1.5} />
              </ActionIcon>
            </Group>
          </FormRow>

          <FormRow
            hint="Префикс станет частью адреса базы. Изменить его после создания нельзя."
            htmlFor="valkey-prefix"
            label="Префикс"
          >
            <Stack gap="h3_xs">
              <TextInput
                id="valkey-prefix"
                maxLength={INSTANCE_PREFIX_MAX_LENGTH}
                placeholder="valkey"
                {...form.getInputProps('prefix')}
              />

              <Text c="h3_text_2" size="h3_xs">
                Адрес базы: {addressPreview}, где после дефиса шесть случайных символов.
              </Text>
            </Stack>
          </FormRow>

          <FormRow
            hint="Разрешает подключения только с указанных публичных IPv4-адресов и подсетей."
            label="Белый список адресов"
          >
            <Stack gap="h3_sm">
              <Switch
                label="Ограничить доступ по IP-адресам"
                {...form.getInputProps('isWhitelistEnabled', { type: 'checkbox' })}
              />

              {values.isWhitelistEnabled && (
                <Textarea
                  description="По одному IPv4-адресу или диапазону CIDR в строке"
                  label="Разрешённые адреса"
                  placeholder={'203.0.113.10\n198.51.100.0/24'}
                  rows={3}
                  {...form.getInputProps('whitelist')}
                />
              )}
            </Stack>
          </FormRow>

          <FormRow>
            <Button
              disabled={!quota.fits}
              mt="h3_md"
              loading={submitting}
              type="submit"
              variant={buttonVariants.accent}
              fullWidth
            >
              Создать базу
            </Button>
          </FormRow>
        </Stack>
      </form>
    </Container>
  );
}
