import type { AnnouncementAudience } from "@/lib/api";

/**
 * What each audience is called on screen.
 *
 * **The enum is two values and one of the labels is three words, because the
 * enum is the wrong sentence.** `publishers` includes administrators — an
 * administrator is a strict superset of a publisher in capability, and
 * everybody who administers an install also puts things on it. An announcement
 * about the build queue that reached every publisher *except the people who run
 * the queue* is the one failure that switch can produce, so the label says who
 * is actually in it rather than exposing the stored word.
 *
 * Its own module rather than a second export from the list component: a file
 * that exports a component and a constant loses fast refresh, and this is read
 * by the list and by the composer.
 */
export const AUDIENCE_LABEL: Record<AnnouncementAudience, string> = {
  all: "Everyone on this install",
  publishers: "Publishers and administrators",
};
