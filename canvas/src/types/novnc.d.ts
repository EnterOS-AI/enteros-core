declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    scaleViewport: boolean;
    resizeSession: boolean;
    focusOnClick: boolean;
    constructor(target: HTMLElement, url: string, options?: { wsProtocols?: string[]; [key: string]: unknown });
    disconnect(): void;
  }
}
