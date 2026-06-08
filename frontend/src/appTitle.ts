const appName = 'Agent Overflow';

type AppTitleEnv = {
  DEV: boolean;
  MODE: string;
};

export function appTitle(isDev: boolean): string {
  if (isDev) {
    return `${appName} (dev)`;
  }
  return appName;
}

export function appTitleForEnv(env: AppTitleEnv): string {
  return appTitle(env.DEV || env.MODE === 'development');
}
