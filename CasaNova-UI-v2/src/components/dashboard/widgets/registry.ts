// Dashboard widget registry — maps a WidgetId to its component + metadata.
// The layout store (stores/widgetLayout.ts) holds order/visibility; this maps
// each id to what actually renders. Components are lazy so unused widgets don't
// weigh on the initial dashboard chunk.
import { defineAsyncComponent, type Component } from 'vue'
import type { WidgetId } from '@/stores/widgetLayout'

export interface WidgetDef {
  id: WidgetId
  // i18n key for the human name shown in the "add widget" picker.
  nameKey: string
  // lucide icon name (resolved by AppIcon) for the picker.
  icon: string
  component: Component
}

export const WIDGET_REGISTRY: Record<WidgetId, WidgetDef> = {
  clock: {
    id: 'clock', nameKey: 'dashboard.widgets.clock.name', icon: 'Clock',
    component: defineAsyncComponent(() => import('../ClockWidget.vue')),
  },
  system: {
    id: 'system', nameKey: 'dashboard.widgets.system.name', icon: 'Activity',
    component: defineAsyncComponent(() => import('../SystemStateWidget.vue')),
  },
  storage: {
    id: 'storage', nameKey: 'dashboard.widgets.storage.name', icon: 'HardDrive',
    component: defineAsyncComponent(() => import('../StorageWidget.vue')),
  },
  network: {
    id: 'network', nameKey: 'dashboard.widgets.network.name', icon: 'Network',
    component: defineAsyncComponent(() => import('../NetworkWidget.vue')),
  },
  vms: {
    id: 'vms', nameKey: 'dashboard.widgets.vms.name', icon: 'Server',
    component: defineAsyncComponent(() => import('./VmsWidget.vue')),
  },
  diskhealth: {
    id: 'diskhealth', nameKey: 'dashboard.widgets.diskhealth.name', icon: 'ShieldCheck',
    component: defineAsyncComponent(() => import('./DiskHealthWidget.vue')),
  },
  traffic: {
    id: 'traffic', nameKey: 'dashboard.widgets.traffic.name', icon: 'ArrowUpDown',
    component: defineAsyncComponent(() => import('./NetworkTrafficWidget.vue')),
  },
  uptime: {
    id: 'uptime', nameKey: 'dashboard.widgets.uptime.name', icon: 'Timer',
    component: defineAsyncComponent(() => import('./UptimeLoadWidget.vue')),
  },
  tasks: {
    id: 'tasks', nameKey: 'dashboard.widgets.tasks.name', icon: 'CalendarClock',
    component: defineAsyncComponent(() => import('./TasksWidget.vue')),
  },
  notifications: {
    id: 'notifications', nameKey: 'dashboard.widgets.notifications.name', icon: 'Bell',
    component: defineAsyncComponent(() => import('./NotificationsWidget.vue')),
  },
}
