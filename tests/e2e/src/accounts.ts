export interface TestAccount {
  email: string;
  password: string;
  registered: boolean;
  instances: Map<string, { name: string; slug: string }>;
}

export class AccountRegistry {
  private readonly accounts: TestAccount[] = [];

  track(email: string, password: string) {
    const account: TestAccount = {
      email,
      password,
      registered: false,
      instances: new Map(),
    };
    this.accounts.push(account);
    return account;
  }

  all() {
    return [...this.accounts];
  }

  markRegistered(account: TestAccount) {
    account.registered = true;
  }

  trackInstance(account: TestAccount, name: string, slug: string) {
    account.instances.set(slug, { name, slug });
  }

  forgetInstance(account: TestAccount, slug: string) {
    account.instances.delete(slug);
  }
}
