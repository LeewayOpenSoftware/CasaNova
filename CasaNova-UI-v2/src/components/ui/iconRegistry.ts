// Icon registry — maps icon names to components from both Lucide and Phosphor.
// Only the icons actually used in the app are imported, keeping the bundle small.
//
// Lucide is loaded eagerly: it is the default pack (stores/iconPack.ts) and it
// is also AppIcon's fallback, so every session needs it. Phosphor is code-split
// into ./iconRegistry.phosphor — it used to ride in this chunk unconditionally,
// ~453 KB of components that a Lucide session never renders.

import { shallowReactive } from 'vue'
import type { Component } from 'vue'
import { getPref } from '@/composables/syncedPrefs'

// Lucide icons (currently used)
import {
  Activity, AlertCircle, AlertOctagon, AlertTriangle, AppWindow, Archive,
  ArrowDown, ArrowLeft, ArrowRight, ArrowUp, ArrowUpCircle, ArrowUpDown,
  Ban, Bell, BellRing, Box, Bug,
  Calendar, CalendarClock, Camera, Check, CheckCircle, CheckCircle2,
  ChevronDown, ChevronLeft, ChevronRight, ChevronUp,
  Circle, CircleAlert, CircleCheck, CircleHelp, CircleX, CircuitBoard,
  ClipboardCopy, ClipboardPaste, Clock, Cloud, CloudUpload, Code2, Container, Copy, Cpu,
  Database, Download,
  Edit, Edit3, ExternalLink, Eye, EyeOff,
  File, FileArchive, FileClock, FileCode, FileQuestion, FileText, Files, Film, Filter, Flame,
  Folder, FolderOpen, FolderPlus, FolderSearch, FolderSync, FolderTree, FolderUp,
  Gauge, GitMerge, Github, Globe,
  HardDrive, Heart,
  Image, Info,
  KeyRound,
  Layers, LayoutGrid, Leaf, Lightbulb, List, Loader, Loader2, Lock, LogIn, LogOut,
  Maximize2, MemoryStick, Minus, Monitor, MonitorPlay, MonitorSmartphone, Moon, MoreHorizontal, MoreVertical, Music,
  NotebookPen, Network,
  Package, Palette, PanelRight, Pencil, Play, Plug, Plus, PlusCircle, Power,
  RefreshCw, RotateCcw, RotateCw,
  Save, Scissors, ScrollText, Search, Server, Settings, Settings2, Share2,
  Shield, ShieldAlert, ShieldCheck, ShieldOff, Smartphone, Sparkles, Square, Store, Sun,
  Tag, Terminal, Thermometer, Trash2,
  Upload, UploadCloud, Usb, User, User2, UserPlus, Users,
  Wifi, Wrench,
  X, XCircle, XOctagon,
  Zap,
  PackageOpen, Link2, Disc3, Stethoscope, Paintbrush, Radio, RefreshCcw,
  TriangleAlert, Undo2, Star, ArrowRightLeft, TerminalSquare, Timer, Pause, ServerOff, MonitorX, Move,
} from 'lucide-vue-next'

export const lucideIcons: Record<string, Component> = {
  Activity, AlertCircle, AlertOctagon, AlertTriangle, AppWindow, Archive,
  ArrowDown, ArrowLeft, ArrowRight, ArrowUp, ArrowUpCircle, ArrowUpDown,
  Ban, Bell, BellRing, Box, Bug,
  Calendar, CalendarClock, Camera, Check, CheckCircle, CheckCircle2,
  ChevronDown, ChevronLeft, ChevronRight, ChevronUp,
  Circle, CircleAlert, CircleCheck, CircleHelp, CircleX, CircuitBoard,
  ClipboardCopy, ClipboardPaste, Clock, Cloud, CloudUpload, Code2, Container, Copy, Cpu,
  Database, Download,
  Edit, Edit3, ExternalLink, Eye, EyeOff,
  File, FileArchive, FileClock, FileCode, FileQuestion, FileText, Files, Film, Filter, Flame,
  Folder, FolderOpen, FolderPlus, FolderSearch, FolderSync, FolderTree, FolderUp,
  Gauge, GitMerge, Github, Globe,
  HardDrive, Heart,
  Image, Info,
  KeyRound,
  Layers, LayoutGrid, Leaf, Lightbulb, List, Loader, Loader2, Lock, LogIn, LogOut,
  Maximize2, MemoryStick, Minus, Monitor, MonitorPlay, MonitorSmartphone, Moon, MoreHorizontal, MoreVertical, Music,
  NotebookPen, Network,
  Package, Palette, PanelRight, Pencil, Play, Plug, Plus, PlusCircle, Power,
  RefreshCw, RotateCcw, RotateCw,
  Save, Scissors, ScrollText, Search, Server, Settings, Settings2, Share2,
  Shield, ShieldAlert, ShieldCheck, ShieldOff, Smartphone, Sparkles, Square, Store, Sun,
  Tag, Terminal, Thermometer, Trash2,
  Upload, UploadCloud, Usb, User, User2, UserPlus, Users,
  Wifi, Wrench,
  X, XCircle, XOctagon,
  Zap,
  PackageOpen, Link2, Disc3, Stethoscope, Paintbrush, Radio, RefreshCcw,
  TriangleAlert, Undo2, Star, ArrowRightLeft, TerminalSquare, Timer, Pause, ServerOff, MonitorX, Move,
}

// Phosphor components land here once ./iconRegistry.phosphor resolves.
// shallowReactive, not reactive: the values are component definitions and have
// to stay raw — the shallow proxy exists only so AppIcon's computed re-runs and
// swaps the Lucide fallback for the real glyph the moment the chunk arrives.
const loadedPhosphor = shallowReactive<Record<string, Component>>({})

let phosphorPending: Promise<void> | null = null

function loadPhosphorPack(): void {
  if (phosphorPending) return
  phosphorPending = import('./iconRegistry.phosphor').then(
    (pack) => {
      Object.assign(loadedPhosphor, pack.default)
    },
    (err) => {
      // A chunk that never arrives must not blank the UI: an empty map keeps
      // AppIcon on its Lucide fallback. Clearing the guard lets the next read
      // retry instead of the session being stuck on one bad response.
      phosphorPending = null
      console.warn('[icons] Phosphor pack failed to load; staying on Lucide', err)
    },
  )
}

// Reading a name is what fetches the pack. AppIcon only indexes this map when
// the user's pack is Phosphor, so the request happens exactly when it is needed
// — including a switch made at runtime from Settings → Appearance, which a
// module-level check on the stored preference would never see.
export const phosphorIcons: Record<string, Component> = new Proxy(loadedPhosphor, {
  get(target, key) {
    if (typeof key === 'string') loadPhosphorPack()
    return target[key as string]
  },
})

// Kick the fetch off during module init when the stored preference already says
// Phosphor, so those sessions don't paint a frame of Lucide glyphs while the
// chunk is in flight. Same key stores/iconPack.ts reads.
if (getPref('casanova.icon_pack') === 'phosphor') loadPhosphorPack()
