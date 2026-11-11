// Decision submission payloads (Designv9 §5.1). The container owns
// the post; this pure step shapes and validates the draft so plain
// node covers the error paths the harness cannot mount: a bad snooze
// date never reaches the network, and text/member rules are explicit.

/**
 * @param {{kind: string, untilText?: string, text?: string, includeMembers?: boolean}} draft
 * @param {{subjectKind: string, subjectId: string, requestId: string, memberScope?: boolean}} ctx
 * @returns {{payload?: Record<string, unknown>, error?: string}}
 */
export function buildDecisionPayload(draft, ctx) {
  if (draft.kind === 'snooze' && (draft.untilText ?? '') !== '') {
    const at = new Date(draft.untilText);
    if (Number.isNaN(at.getTime())) {
      return { error: 'snooze time is not a date' };
    }
    return { payload: { ...base(ctx, draft), until: at.toISOString() } };
  }
  const payload = base(ctx, draft);
  if (draft.kind === 'note' || draft.kind === 'label') {
    payload.text = draft.text ?? '';
  }
  if (ctx.memberScope) {
    payload.includeMembers = draft.includeMembers ?? false;
  }
  return { payload };
}

function base(ctx, draft) {
  return {
    subjectKind: ctx.subjectKind,
    subjectId: ctx.subjectId,
    kind: draft.kind,
    requestId: ctx.requestId
  };
}
