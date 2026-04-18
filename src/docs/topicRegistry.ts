import gettingStarted from './getting-started.md';
import calendar from './calendar.md';
import makingBookings from './making-bookings.md';
import gpuResources from './gpu-resources.md';
import kueue from './kueue.md';
import admin from './admin.md';
import slotsAndConflicts from './slots-and-conflicts.md';
import faq from './faq.md';

export interface HelpTopic {
  slug: string;
  title: string;
  section: 'User Guide' | 'Admin' | 'Reference';
  content: string;
}

export const topics: HelpTopic[] = [
  { slug: 'getting-started', title: 'Getting Started', section: 'User Guide', content: gettingStarted },
  { slug: 'calendar', title: 'Calendar & Navigation', section: 'User Guide', content: calendar },
  { slug: 'making-bookings', title: 'Making Bookings', section: 'User Guide', content: makingBookings },
  { slug: 'gpu-resources', title: 'GPU Resources', section: 'User Guide', content: gpuResources },
  { slug: 'kueue', title: 'Kueue & Auto-Bookings', section: 'User Guide', content: kueue },
  { slug: 'admin', title: 'Admin Dashboard', section: 'Admin', content: admin },
  { slug: 'slots-and-conflicts', title: 'Slots & Conflicts', section: 'Reference', content: slotsAndConflicts },
  { slug: 'faq', title: 'FAQ', section: 'Reference', content: faq },
];

export const sections = ['User Guide', 'Admin', 'Reference'] as const;

export function getTopicBySlug(slug: string): HelpTopic | undefined {
  return topics.find((t) => t.slug === slug);
}
