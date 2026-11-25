# Append-only record of one review action on a submission.
class StruggleReviewDecision < ApplicationRecord
  belongs_to :struggle_submission
  belongs_to :reviewer, class_name: "User"

  validates :action, inclusion: { in: StruggleSubmission::ACTIONS.keys }
  validates :justification, presence: true

  def readonly? = persisted?
end
