import type { Page } from "@playwright/test";

type GotoOptions = Parameters<Page["goto"]>[1];

type PageWithGoto<T> = {
  goto: (url: string, options?: GotoOptions) => Promise<T>;
};

const NETWORK_CHANGED = "net::ERR_NETWORK_CHANGED";
const RETRY_DELAY_MS = 250;

export async function gotoWithNetworkChangeRetry<T>(
  page: PageWithGoto<T>,
  url: string,
  options?: GotoOptions,
  retryDelayMs = RETRY_DELAY_MS,
): Promise<T> {
  try {
    return await page.goto(url, options);
  } catch (error) {
    if (!(error instanceof Error) || !error.message.includes(NETWORK_CHANGED)) {
      throw error;
    }
  }

  await new Promise((resolve) => setTimeout(resolve, retryDelayMs));
  return page.goto(url, options);
}
