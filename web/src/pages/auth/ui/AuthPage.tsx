import { useEffect, useState } from 'react';
import { AlertTriangle, ArrowLeft, KeyRound } from 'lucide-react';
import { useLoaderData, useNavigate } from 'react-router';
import { Anchor, Button, PasswordInput, Stack, Text, TextInput, Title } from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError, checkEmail, login, register } from '@/shared/api';
import { buttonVariants, routes } from '@/shared/config';
import { setRybbitUser } from '@/shared/lib';
import { LogoWide } from '@/shared/ui';
import { getEmailRequestError } from '../model/auth-form';
import { useCapsLock } from '../model/use-caps-lock';
import styles from './AuthPage.module.css';

type AuthStep = 'email' | 'login' | 'register';

interface AuthRouteData {
  reason: string | null;
  returnTo: string;
}

interface AuthFormValues {
  confirmPassword: string;
  email: string;
  password: string;
}

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const CYRILLIC_PATTERN = /[А-Яа-яЁё]/;

function getErrorMessage(error: unknown) {
  return error instanceof ApiError
    ? error.message
    : 'Не удалось выполнить запрос. Попробуйте ещё раз.';
}

/**
 * Область подсказок остаётся в разметке всегда, даже пустой: живую область
 * читает вспомогательная техника, а вставленную заново она пропускает.
 *
 * CapsLock один на всю клавиатуру, поэтому о нём предупреждает только первое поле
 * пароля. Раскладку каждое поле проверяет по своему значению.
 */
function PasswordHints({ capsLock = false, value }: { capsLock?: boolean; value: string }) {
  return (
    <Stack aria-live="polite" className={styles.hints} gap="h3_xs">
      {capsLock && (
        <Text c="h3_text_2" size="h3_sm" className={styles.hint}>
          <KeyRound aria-hidden="true" size={16} strokeWidth={1.5} />
          Включён CapsLock
        </Text>
      )}

      {CYRILLIC_PATTERN.test(value) && (
        <Text c="h3_text_2" size="h3_sm" className={styles.hint}>
          <AlertTriangle aria-hidden="true" size={16} strokeWidth={1.5} />
          Возможно, включена русская раскладка
        </Text>
      )}
    </Stack>
  );
}

export function AuthPage() {
  const { reason, returnTo } = useLoaderData() as AuthRouteData;
  const navigate = useNavigate();

  const [step, setStep] = useState<AuthStep>('email');
  const [loading, setLoading] = useState(false);
  const [stepMessage, setStepMessage] = useState<string | null>(null);

  const capsLock = useCapsLock();

  useEffect(() => {
    setRybbitUser(null);
  }, []);

  const form = useForm<AuthFormValues>({
    mode: 'controlled',
    initialValues: { confirmPassword: '', email: '', password: '' },
    validateInputOnBlur: true,
    validate: (values) => ({
      email: EMAIL_PATTERN.test(values.email.trim())
        ? null
        : 'Введите адрес почты в верном формате',
      password:
        step !== 'email' && values.password.length < 8
          ? 'Пароль должен содержать не меньше 8 символов'
          : null,
      confirmPassword:
        step === 'register' && values.confirmPassword !== values.password
          ? 'Пароли не совпадают'
          : null,
    }),
  });

  useEffect(() => {
    if (reason === 'session-expired') {
      notifications.show({
        color: 'red',
        message: 'Срок действия сессии закончился. Войдите снова.',
        title: 'Сессия истекла',
      });
    }
  }, [reason]);

  const submitEmail = async (values: AuthFormValues) => {
    setLoading(true);
    setStepMessage(null);

    try {
      const result = await checkEmail({ email: values.email });

      form.setFieldValue('email', values.email.trim().toLowerCase());
      setStep(result.exists ? 'login' : 'register');
    } catch (error) {
      form.setFieldError('email', getEmailRequestError(error));
    } finally {
      setLoading(false);
    }
  };

  const submitPassword = async (values: AuthFormValues) => {
    setLoading(true);
    setStepMessage(null);

    try {
      if (step === 'register') {
        await register({ email: values.email, password: values.password });
      } else {
        await login({ email: values.email, password: values.password });
      }

      await navigate(returnTo || routes.home, { replace: true });
    } catch (error) {
      form.setFieldValue('password', '');
      form.setFieldValue('confirmPassword', '');

      if (step === 'register' && error instanceof ApiError && error.code === 'CONFLICT') {
        setStep('login');
        setStepMessage('Аккаунт с этой почтой уже появился. Введите пароль для входа.');
      } else {
        form.setFieldError('password', getErrorMessage(error));
      }
    } finally {
      setLoading(false);
    }
  };

  const returnToEmail = () => {
    form.setFieldValue('password', '');
    form.setFieldValue('confirmPassword', '');

    form.clearFieldError('password');
    form.clearFieldError('confirmPassword');

    setStepMessage(null);
    setStep('email');
  };

  const title =
    step === 'email' ? 'Вход в консоль' : step === 'login' ? 'Введите пароль' : 'Создайте аккаунт';

  const description =
    step === 'email'
      ? 'Введите почту. Если аккаунта ещё нет, мы предложим его создать.'
      : step === 'login'
        ? 'Введите пароль от аккаунта.'
        : 'Задайте пароль не короче 8 символов.';

  const submitLabel =
    step === 'email' ? 'Продолжить' : step === 'login' ? 'Войти' : 'Создать аккаунт';

  return (
    <main className={styles.page}>
      <div className={styles.content}>
        <LogoWide className={styles.logo} width={176} />

        <section className={styles.card} aria-labelledby="auth-title">
          <form
            noValidate
            onSubmit={form.onSubmit(step === 'email' ? submitEmail : submitPassword)}
          >
            <Stack gap="h3_md">
              <div>
                <Title id="auth-title" order={2}>
                  {title}
                </Title>

                <Text c="h3_text_2" mt="h3_xs" size="h3_sm">
                  {description}
                </Text>
              </div>

              {step === 'email' ? (
                <TextInput
                  autoComplete="email"
                  label="Почта"
                  placeholder="you@example.com"
                  type="email"
                  {...form.getInputProps('email')}
                />
              ) : (
                <>
                  <TextInput
                    autoComplete="username"
                    classNames={{ input: styles.email }}
                    label="Почта"
                    readOnly
                    {...form.getInputProps('email')}
                  />

                  {stepMessage && (
                    <Text c="h3_text_2" size="h3_sm">
                      {stepMessage}
                    </Text>
                  )}

                  <div>
                    <PasswordInput
                      autoComplete={step === 'login' ? 'current-password' : 'new-password'}
                      label="Пароль"
                      {...form.getInputProps('password')}
                    />
                    <PasswordHints capsLock={capsLock} value={form.values.password} />
                  </div>

                  {step === 'register' && (
                    <div>
                      <PasswordInput
                        autoComplete="new-password"
                        label="Повторите пароль"
                        {...form.getInputProps('confirmPassword')}
                      />
                      <PasswordHints value={form.values.confirmPassword} />
                    </div>
                  )}
                </>
              )}

              <Button fullWidth loading={loading} type="submit" variant={buttonVariants.accent}>
                {submitLabel}
              </Button>

              {step !== 'email' && (
                <Anchor component="button" onClick={returnToEmail} type="button" underline="hover">
                  <ArrowLeft aria-hidden="true" size={16} strokeWidth={1.5} />
                  {' Сменить почту'}
                </Anchor>
              )}
            </Stack>
          </form>
        </section>
      </div>
    </main>
  );
}
