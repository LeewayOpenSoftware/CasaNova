// Minimal type surface for the noVNC RFB client (the package ships no types).
// Covers only what VncConsole.vue uses.
declare module '@novnc/novnc' {
  export interface RFBOptions {
    credentials?: { username?: string; password?: string; target?: string }
    shared?: boolean
    repeaterID?: string
    wsProtocols?: string[]
  }
  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, urlOrDataChannel: string, options?: RFBOptions)
    scaleViewport: boolean
    resizeSession: boolean
    clipViewport: boolean
    viewOnly: boolean
    background: string
    disconnect(): void
    sendCtrlAltDel(): void
    sendKey(keysym: number, code: string, down?: boolean): void
    focus(): void
    machineReboot(): void
    machineShutdown(): void
  }
}
