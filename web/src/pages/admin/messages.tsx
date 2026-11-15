import { PageHeader } from "@/components/page-header";
import { AnnouncementEditor } from "@/components/announcement-editor";
import { AccessMessagesEditor } from "@/components/access-messages-editor";
import { KnowledgeBasePicker } from "@/components/knowledge-base-picker";
import { AnnouncementEmailList } from "@/components/announcement-email-list";
import { useHashScroll } from "@/hooks/use-hash-scroll";

/**
 * Admin → Messages: everything this platform says in an administrator's voice.
 *
 * Two editors that were the last two cards on Settings (ADR §50 D2). Neither is
 * a platform capability — nothing is turned on or off here — and both already
 * saved themselves, which made them the clearest violation of one save model
 * per page: a page-wide "3 unsaved changes" bar that committed neither of them.
 *
 * They are together because the argument that put them on Settings was for one
 * place, and this gives them one. What is on the page is the whole of what this
 * install says in its own voice: a banner everybody sees, the announcements it
 * sends to people who are not looking, the two sentences somebody sees instead
 * of getting in, and the one app it points everybody at.
 *
 * The banner and the announcements are neighbours and are not the same thing,
 * which is why they are two sections rather than one control with a checkbox.
 * A banner is a *state* that has to be cleared; an announcement is an *event*
 * that is over once it is sent. One switch writing both would leave a banner
 * nobody remembers publishing.
 *
 * The knowledge base is here on the strength of the page's subject rather than
 * its name. It is not a message, but it is the same act — the administrator
 * saying *read this* — and the alternative was a fourteenth section of Settings,
 * which is the page this one exists to have left.
 */
export function AdminMessagesPage() {
  useHashScroll();

  return (
    <div>
      <PageHeader
        title="Messages"
        description="What people read in banners, announcement emails and sign-in refusals."
      />

      <div className="space-y-6">
        <section id="announcement" className="scroll-mt-20">
          <AnnouncementEditor />
        </section>
        <section id="announcement-emails" className="scroll-mt-20">
          <AnnouncementEmailList />
        </section>
        <section id="access" className="scroll-mt-20">
          <AccessMessagesEditor />
        </section>
        <section id="knowledge-base" className="scroll-mt-20">
          <KnowledgeBasePicker />
        </section>
      </div>
    </div>
  );
}
