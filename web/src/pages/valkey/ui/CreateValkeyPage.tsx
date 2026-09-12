import { useCallback, useEffect, useRef, useState } from 'react';
import { RefreshCw, RotateCw } from 'lucide-react';
import { useNavigate } from 'react-router';
import {
  ActionIcon,
  Alert,
  Anchor,
  Button,
  Checkbox,
  Container,
  Group,
  NumberInput,
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
import { createUuidV7 } from '@/shared/lib';
import {
  createInstance,
  getValkeyCatalog,
  listInstances,
  type CreateInstanceInput,
} from '../api/valkey-api';
import {
  findLargestAvailableCapacitySize,
  type QuotaAmount,
  type QuotaCheck,
} from '../model/quota';
import {
  formatRam,
  formatVcpu,
  generateInstanceName,
  getTotalResources,
  getSlugPreview,
  INSTANCE_PREFIX_MAX_LENGTH,
  validateInstanceName,
  validateInstancePrefix,
  parseWhitelistCidrs,
  validateWhitelistCidrs,
  type PricePeriod,
  type ValkeyCatalog,
  type ValkeyInstance,
  type ValkeyMode,
  type ValkeySize,
} from '../model/valkey';
import { useValkeyCapacityPolling } from '../model/valkey-capacity-polling';
import { generateValkeyPassword } from '../model/valkey-credentials';
import {
  checkCandidateCapacity,
  getFieldErrors,
  getCreateFormDefaults,
  getRequestErrorMessage,
  shouldReuseSubmission,
  SUPPORT_URL,
  validateMaintenanceDow,
  validateMaintenanceDurationMin,
  validateMaintenanceHourUtc,
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
  limit: QuotaAmount;
  quota: QuotaCheck;
  singleMaximum: ValkeySize | null;
}

function SupportNote({ haMaximum, limit, quota, singleMaximum }: SupportNoteProps) {
  return (
    <Alert className={styles.quotaAlert} color="red" title="Недостаточно квоты" variant="light">
      <Stack align="flex-start" gap="h3_sm">
        <Stack gap="h3_xs">
          <Text size="h3_sm">Ваша квота: {formatResources(limit)}.</Text>
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
  const [catalog, setCatalog] = useState<ValkeyCatalog | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [period, setPeriod] = useState<PricePeriod>('month');
  const [submitting, setSubmitting] = useState(false);
  const defaultsAppliedRef = useRef(false);
  const loadControllerRef = useRef<AbortController | null>(null);
  const submissionControllerRef = useRef<AbortController | null>(null);
  const pendingSubmissionRef = useRef<{
    fingerprint: string;
    idempotencyKey: string;
    input: CreateInstanceInput;
  } | null>(null);
  const {
    capacity: capacitySnapshot,
    error: capacityError,
    refresh: refreshCapacity,
  } = useValkeyCapacityPolling(session.userId);

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
      confirmDenyAll: false,
      maintenanceEnabled: false,
      maintenanceDow: 0,
      maintenanceHourUtc: 0,
      maintenanceDurationMin: 30,
    },
    validateInputOnBlur: true,
    validate: {
      name: (value) => validateInstanceName(value.trim()),
      prefix: (value) => validateInstancePrefix(value.trim()),
      whitelist: (value, formValues) =>
        formValues.isWhitelistEnabled ? validateWhitelistCidrs(value) : null,
      confirmDenyAll: (value, formValues) =>
        formValues.isWhitelistEnabled &&
        parseWhitelistCidrs(formValues.whitelist).length === 0 &&
        !value
          ? 'Подтвердите закрытие доступа всем'
          : null,
      maintenanceDow: (value, formValues) =>
        validateMaintenanceDow(value, formValues.maintenanceEnabled),
      maintenanceHourUtc: (value, formValues) =>
        validateMaintenanceHourUtc(value, formValues.maintenanceEnabled),
      maintenanceDurationMin: (value, formValues) =>
        validateMaintenanceDurationMin(value, formValues.maintenanceEnabled),
    },
  });

  const { setValues } = form;

  const load = useCallback(
    async (signal: AbortSignal) => {
      setLoading(true);
      setLoadError(null);

      const [instancesResult, catalogResult] = await Promise.allSettled([
        listInstances(signal),
        getValkeyCatalog(signal),
      ]);
      if (signal.aborted) {
        return;
      }

      if (instancesResult.status === 'fulfilled') {
        setInstances(instancesResult.value);
      }
      if (catalogResult.status === 'fulfilled') {
        setCatalog(catalogResult.value);
      }
      const failed = [instancesResult, catalogResult].find(
        (result): result is PromiseRejectedResult =>
          result.status === 'rejected' &&
          !(result.reason instanceof DOMException && result.reason.name === 'AbortError')
      );
      if (failed) {
        setLoadError(getRequestErrorMessage(failed.reason));
      }
      setLoading(false);
    },
    [session.userId, setValues]
  );

  useEffect(() => {
    if (defaultsAppliedRef.current || !instances || !capacitySnapshot) {
      return;
    }

    setValues(
      getCreateFormDefaults(instances, catalog?.items ?? [{ vcpu: 1, ramGb: 1 }], capacitySnapshot)
    );
    defaultsAppliedRef.current = true;
  }, [capacitySnapshot, catalog, instances, setValues]);

  const retryLoad = () => {
    loadControllerRef.current?.abort();
    const controller = new AbortController();
    loadControllerRef.current = controller;
    void load(controller.signal);
  };

  useEffect(() => {
    setTrailingCrumb('Создание БД');
    return () => setTrailingCrumb(null);
  }, [setTrailingCrumb]);

  useEffect(() => {
    defaultsAppliedRef.current = false;
    setInstances(null);
    setCatalog(null);
    const controller = new AbortController();
    loadControllerRef.current = controller;
    void load(controller.signal);

    return () => {
      loadControllerRef.current?.abort();
    };
  }, [load]);

  useEffect(() => {
    const clearPending = () => {
      submissionControllerRef.current?.abort();
      submissionControllerRef.current = null;
      pendingSubmissionRef.current = null;
    };
    window.addEventListener('pagehide', clearPending);
    return () => {
      window.removeEventListener('pagehide', clearPending);
      clearPending();
    };
  }, [session.userId]);

  const values = form.getValues();
  const size = { vcpu: values.vcpu, ramGb: values.ramGb };
  const capacity = capacitySnapshot
    ? checkCandidateCapacity(capacitySnapshot, { size, mode: values.mode })
    : null;
  const singleMaximum =
    catalog && capacitySnapshot
      ? findLargestAvailableCapacitySize(catalog.items, capacitySnapshot, 'single')
      : null;
  const haMaximum =
    catalog && capacitySnapshot
      ? findLargestAvailableCapacitySize(catalog.items, capacitySnapshot, 'ha')
      : null;

  const slugPreview = getSlugPreview(values.prefix.trim() || 'valkey');
  const primaryAddressPreview = catalog
    ? `redis://${slugPreview}.${catalog.connection.domain}:${catalog.connection.port}`
    : `redis://${slugPreview}`;
  const readAddressPreview = catalog
    ? `redis://${slugPreview}-ro.${catalog.connection.domain}:${catalog.connection.port}`
    : `redis://${slugPreview}-ro`;
  const needsDenyAllConfirmation =
    values.isWhitelistEnabled &&
    parseWhitelistCidrs(values.whitelist).length === 0 &&
    !values.confirmDenyAll;

  const submit = async (formValues: CreateFormValues) => {
    setSubmitting(true);
    const input: CreateInstanceInput = {
      name: formValues.name.trim(),
      prefix: formValues.prefix.trim(),
      mode: formValues.mode,
      vcpu: formValues.vcpu,
      ramGb: formValues.ramGb,
      password: '',
      isWhitelistEnabled: formValues.isWhitelistEnabled,
      whitelistCidrs: formValues.isWhitelistEnabled
        ? parseWhitelistCidrs(formValues.whitelist)
        : [],
      maintenance: formValues.maintenanceEnabled
        ? {
            dow: formValues.maintenanceDow,
            hourUtc: formValues.maintenanceHourUtc,
            durationMin: formValues.maintenanceDurationMin,
          }
        : null,
    };
    const fingerprint = JSON.stringify({ ...input, password: undefined });
    const existing = pendingSubmissionRef.current;
    const submission =
      existing?.fingerprint === fingerprint
        ? existing
        : {
            fingerprint,
            idempotencyKey: createUuidV7(),
            input: { ...input, password: generateValkeyPassword() },
          };
    pendingSubmissionRef.current = submission;
    const controller = new AbortController();
    submissionControllerRef.current = controller;

    try {
      const created = await createInstance(
        submission.input,
        submission.idempotencyKey,
        controller.signal
      );
      if (submissionControllerRef.current !== controller) {
        return;
      }

      pendingSubmissionRef.current = null;
      void refreshCapacity();
      setEphemeralPassword({
        instanceId: created.id,
        password: submission.input.password,
        source: 'creation',
      });
      await navigate(valkeyInstancePath(created.id));
    } catch (error) {
      if (
        submissionControllerRef.current !== controller ||
        (error instanceof DOMException && error.name === 'AbortError')
      ) {
        return;
      }
      if (!shouldReuseSubmission(error)) {
        pendingSubmissionRef.current = null;
      }
      if (error instanceof ApiError) {
        const fields = getFieldErrors(error);
        for (const field of ['name', 'prefix', 'mode'] as const) {
          if (fields[field]) {
            form.setFieldError(field, error.message);
          }
        }
        if (error.code === 'CONFLICT' && error.details?.field === 'name') {
          form.setFieldError('name', error.message);
        }
        if (error.code === 'QUOTA_EXCEEDED' || error.code === 'NOT_ENOUGH_RESOURCES') {
          await refreshCapacity();
        }
      }
      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось создать базу',
      });
    } finally {
      if (submissionControllerRef.current === controller) {
        submissionControllerRef.current = null;
      }
      setSubmitting(false);
    }
  };

  const initialError = loadError ?? (capacityError ? getRequestErrorMessage(capacityError) : null);

  if (initialError && (!instances || !capacitySnapshot)) {
    return (
      <Container className={styles.page} size="h3_page">
        <Alert color="red" title="Не удалось открыть форму">
          <Stack align="flex-start" gap="h3_sm">
            <Text size="h3_sm">{initialError}</Text>
            <Button
              leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
              onClick={() => {
                retryLoad();
                void refreshCapacity();
              }}
              variant={buttonVariants.secondary}
            >
              Повторить
            </Button>
          </Stack>
        </Alert>
      </Container>
    );
  }

  if (!instances || !capacitySnapshot || !capacity || (loading && !defaultsAppliedRef.current)) {
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
        header={catalog ? <PricePeriodTabs onChange={setPeriod} period={period} /> : undefined}
        label="Стоимость"
      >
        {catalog ? (
          <PricePanel mode={values.mode} period={period} pricing={catalog.pricing} size={size} />
        ) : (
          <Text c="h3_text_2" size="h3_sm">
            Стоимость появится после загрузки каталога.
          </Text>
        )}
      </ValkeyAside>

      <Title className={styles.pageTitle} order={1}>
        Новая Valkey база
      </Title>

      {loadError ? (
        <Alert color="red" mb="h3_lg" title="Не удалось загрузить каталог">
          <Stack align="flex-start" gap="h3_sm">
            <Text size="h3_sm">{loadError}</Text>
            <Button
              leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
              loading={loading}
              onClick={retryLoad}
              variant={buttonVariants.secondary}
            >
              Повторить
            </Button>
          </Stack>
        </Alert>
      ) : null}

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

          {catalog ? (
            <SizePlans
              getAvailabilityReason={(plan) =>
                checkCandidateCapacity(capacitySnapshot, { size: plan, mode: values.mode }).reason
              }
              mode={values.mode}
              onChange={(next) => setValues({ vcpu: next.vcpu, ramGb: next.ramGb })}
              plans={catalog.items}
              pricing={catalog.pricing}
              size={size}
            />
          ) : null}

          {catalog && capacity.reason === 'user_quota' && (
            <SupportNote
              haMaximum={haMaximum}
              limit={capacitySnapshot.user.limit}
              quota={capacity.user}
              singleMaximum={singleMaximum}
            />
          )}

          {capacity.reason === 'cluster_resources' ? (
            <Alert color="red" title="Недостаточно ресурсов Managed Kubernetes" variant="light">
              <Text size="h3_sm">В кластере сейчас нет ресурсов для выбранной конфигурации.</Text>
            </Alert>
          ) : null}

          {capacity.reason === 'instance_limit' ? (
            <Alert color="red" title="Достигнут предел числа баз" variant="light">
              <Text size="h3_sm">
                Новую базу можно создать после подтверждённого удаления одной из существующих.
              </Text>
            </Alert>
          ) : null}

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
                Primary: {primaryAddressPreview}. Для чтения: {readAddressPreview}. После дефиса
                будут шесть случайных символов.
              </Text>
            </Stack>
          </FormRow>

          <FormRow
            hint="Разрешает подключения только с указанных публичных IPv4-адресов и подсетей."
            label="Белый список"
          >
            <Stack gap="h3_sm">
              <Switch
                label="Ограничить доступ по IP-адресам"
                {...form.getInputProps('isWhitelistEnabled', { type: 'checkbox' })}
              />

              {values.isWhitelistEnabled && (
                <Stack gap="h3_sm">
                  <Textarea
                    description="По одному IPv4-адресу или диапазону CIDR в строке"
                    label="Разрешённые адреса"
                    placeholder={'203.0.113.10\n198.51.100.0/24'}
                    rows={3}
                    {...form.getInputProps('whitelist')}
                  />

                  {parseWhitelistCidrs(values.whitelist).length === 0 ? (
                    <Checkbox
                      label="Запретить все подключения к базе"
                      {...form.getInputProps('confirmDenyAll', { type: 'checkbox' })}
                    />
                  ) : null}
                </Stack>
              )}
            </Stack>
          </FormRow>

          <FormRow
            hint="Окно можно изменить после создания базы. Время указывается в UTC."
            label="Окно обслуживания"
          >
            <Stack gap="h3_sm">
              <Switch
                label="Задать окно обслуживания"
                {...form.getInputProps('maintenanceEnabled', { type: 'checkbox' })}
              />

              {values.maintenanceEnabled ? (
                <SimpleGrid cols={{ base: 1, mobile: 3 }} spacing="h3_sm">
                  <NumberInput
                    label="День недели, 0–6"
                    max={6}
                    min={0}
                    {...form.getInputProps('maintenanceDow')}
                  />
                  <NumberInput
                    label="Час UTC, 0–23"
                    max={23}
                    min={0}
                    {...form.getInputProps('maintenanceHourUtc')}
                  />
                  <NumberInput
                    label="Длительность, минуты"
                    max={1440}
                    min={1}
                    {...form.getInputProps('maintenanceDurationMin')}
                  />
                </SimpleGrid>
              ) : null}
            </Stack>
          </FormRow>

          <FormRow>
            <Button
              disabled={!catalog || !capacity.fits || needsDenyAllConfirmation}
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
