// A response page names generated audit rows; it is not a count of
// currently effective verdicts. Undo of the parent never cascades.
export function memberUndoReview(parentId, result) {
  return {
    parentId,
    count: Array.isArray(result.generated_decision_ids) ? result.generated_decision_ids.length : 0,
    next: result.generated_next ?? 0,
    members: []
  };
}

export function memberUndoNote(review) {
  const count = review.next ? `At least ${review.count}` : String(review.count);
  return `Undone. ${count} member decision${review.count === 1 ? ' was' : 's were'} generated; incident undo does not undo them. Review them on their briefing pages.`;
}

export async function continueMemberUndo(review, fetchPage) {
  const page = await fetchPage(review.parentId, review.next);
  const next = page.next ?? 0;
  if (next !== 0 && next <= review.next) {
    throw new Error('Member decision page did not advance. Try again.');
  }
  return {
    parentId: review.parentId,
    count: review.count + page.members.length,
    next,
    members: page.members
  };
}
