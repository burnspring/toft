/**
 * The one sentence the knowledge base card says about itself when nobody has
 * written a better one.
 *
 * It lives here rather than in the card because the administrator's own field
 * shows it as its placeholder: the person deciding whether to replace it is
 * shown exactly what they would be replacing, and the two cannot drift apart
 * into a product that promises one sentence and renders another.
 *
 * Blank is the ordinary state, and it stays blank — an install that never
 * opens this field reads exactly as it did before the field existed. Nothing
 * seeds the stored value with this string, because a stored copy would keep
 * rendering the old sentence after the product had changed its own.
 */
export const KNOWLEDGE_BASE_DEFAULT_DESCRIPTION =
  "Your organization’s own guides, kept on this install. Opens in a new tab.";

/** Matches `knowledgeBaseDescriptionMaxLen` in internal/api. Two lines in the
 *  card, and short enough that it cannot become the page. */
export const KNOWLEDGE_BASE_DESCRIPTION_MAX = 200;
