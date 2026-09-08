import {
  readCredentialsSnapshot,
  rotatePasswordSnapshot,
  type MutationAuthor,
} from './valkey-storage';

const RESPONSE_DELAY_MS = 400;

const delay = () => new Promise((resolve) => setTimeout(resolve, RESPONSE_DELAY_MS));

export async function getValkeyCredentials(ownerId: string, instanceId: string) {
  await delay();
  return readCredentialsSnapshot(ownerId, instanceId);
}

export async function rotateValkeyPassword(
  author: MutationAuthor,
  instanceId: string,
  input: { password: string; expectedPasswordVersion: number }
) {
  await delay();
  return rotatePasswordSnapshot(author, instanceId, input);
}
