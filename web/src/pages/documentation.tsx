import { useQuery } from "@tanstack/react-query";
import { BookMarked, BookOpen, ExternalLink } from "lucide-react";
import { Link } from "react-router-dom";

import { api, type DocumentationSection } from "@/lib/api";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { QueryError } from "@/components/query-error";
import { SectionHeading } from "@/components/section";
import { Notice } from "@/components/notice";
import { appUrl } from "@/lib/app-url";
import { useAuth } from "@/hooks/use-auth";
import { KNOWLEDGE_BASE_DEFAULT_DESCRIPTION } from "@/lib/knowledge-base";

/**
 * Documentation — Toft's guides, and this install's knowledge base.
 *
 * The guides are one public site, a folder per minor version, and this install
 * links to the folder for the version it runs (ADR, *The manual*). The server
 * composes the links — `GET /documentation` — so this page never builds a URL
 * and cannot disagree with the version it is about.
 *
 * Everybody gets the same three, the admin guide included: it is public, and
 * hiding a link to a public page is not a control. The links open in a new tab
 * with `noreferrer`, so the docs host is not told which install a reader came
 * from.
 *
 * Under `DOCS_URL=off` there are no links, and the page says so without naming
 * anybody to ask: an operator chose it, and nothing on this page can change it.
 */
export function DocumentationPage() {
  const { user } = useAuth();
  const isAdmin = user?.system_role === "system_admin";

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["documentation"],
    queryFn: api.listDocumentation,
  });
  // The same query, by key, as the card below — so the guides-off notice can
  // tell whether a knowledge base already stands above it.
  const { data: kb } = useQuery({
    queryKey: ["knowledge-base"],
    queryFn: api.getKnowledgeBase,
    retry: false,
    staleTime: 5 * 60_000,
  });
  const hasKnowledgeBase = !!kb?.slug;

  return (
    <div>
      <PageHeader
        title="Documentation"
        description={data?.version ? `Guides for Toft ${data.version}.` : undefined}
      />

      <KnowledgeBaseCard />

      {/* The control that sets this is on Admin → Messages, and it stays there:
          the settings index only indexes pages the admin rail can reach, and
          /docs is not one. So this page carries the *route* to it instead. */}
      {isAdmin && (
        <p className="mb-6 text-xs text-muted-foreground">
          Choose this install’s knowledge base on{" "}
          <Link
            to="/admin/messages#knowledge-base"
            className="underline underline-offset-2 hover:text-foreground"
          >
            Admin → Messages
          </Link>
          .
        </p>
      )}

      {error ? (
        <QueryError
          error={error}
          onRetry={() => refetch()}
          title="The guides could not be read"
          fallback="Reload the page. If it keeps failing, tell an administrator."
        />
      ) : isLoading ? (
        <div className="grid gap-4 sm:grid-cols-3">
          <Skeleton className="h-28 w-full" />
          <Skeleton className="h-28 w-full" />
          <Skeleton className="h-28 w-full" />
        </div>
      ) : !data?.url ? (
        // Under a knowledge-base card, "Documentation isn’t available" sat
        // beneath a page titled Documentation that had just offered some.
        hasKnowledgeBase ? (
          <p className="text-sm text-muted-foreground">
            Toft’s own guides are turned off on this install.
          </p>
        ) : (
          <Notice tone="neutral" size="md" title="Documentation isn’t available on this install">
            This install is set up without links to Toft’s documentation.
          </Notice>
        )
      ) : (
        <div className="space-y-3">
          <SectionHeading title="Guides" />
          <div className="grid gap-4 sm:grid-cols-3">
            {data.sections.map((s) => (
              <GuideCard key={s.id} section={s} />
            ))}
          </div>
          {data.agent_skills_url && (
            <p className="pt-1 text-sm text-muted-foreground">
              Using a coding agent?{" "}
              <a
                href={data.agent_skills_url}
                target="_blank"
                rel="noopener noreferrer"
                className="font-medium text-primary hover:underline"
              >
                Install the Toft skills
              </a>{" "}
              so it can build and deploy apps here.
            </p>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * One guide: its name is the link, because that is the click somebody was
 * always going to try.
 */
function GuideCard({ section }: { section: DocumentationSection }) {
  return (
    <Card className="h-full">
      <CardContent className="flex h-full items-start gap-3 py-4">
        <BookOpen className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <div className="min-w-0">
          <a
            href={section.url}
            target="_blank"
            rel="noopener noreferrer"
            className="group flex items-center gap-1.5"
          >
            <span className="text-sm font-semibold text-foreground transition-colors group-hover:text-primary">
              {section.name}
            </span>
            <ExternalLink
              className="h-3.5 w-3.5 shrink-0 text-muted-foreground transition-colors group-hover:text-primary"
              aria-hidden
            />
          </a>
          <p className="mt-1 text-sm text-muted-foreground">{section.summary}</p>
        </div>
      </CardContent>
    </Card>
  );
}

/**
 * The install's own knowledge base — one app an administrator points everybody
 * at. Absent when nothing is picked, and absent for anybody who could not
 * already read the app: the pick is not a share.
 */
function KnowledgeBaseCard() {
  const { data } = useQuery({
    queryKey: ["knowledge-base"],
    queryFn: api.getKnowledgeBase,
    retry: false,
    staleTime: 5 * 60_000,
  });

  if (!data?.slug) return null;

  return (
    <Card className="mb-6">
      <CardContent className="py-4">
        <div className="flex items-start gap-3">
          <BookMarked className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
          <div>
            <a
              href={appUrl(data.slug)}
              target="_blank"
              rel="noopener noreferrer"
              className="group flex items-center gap-1.5"
            >
              <span className="text-sm font-semibold text-foreground transition-colors group-hover:text-primary">
                {data.label || "Knowledge base"}
              </span>
              <ExternalLink
                className="h-3.5 w-3.5 shrink-0 text-muted-foreground transition-colors group-hover:text-primary"
                aria-hidden
              />
            </a>
            <p className="mt-1 text-sm text-muted-foreground">
              {data.description || KNOWLEDGE_BASE_DEFAULT_DESCRIPTION}
            </p>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}
