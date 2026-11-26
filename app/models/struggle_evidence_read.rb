# Log of someone other than the submitter opening a submission's evidence.
class StruggleEvidenceRead < ApplicationRecord
  belongs_to :struggle_submission
  belongs_to :user

  validates :scope, inclusion: { in: %w[review escalated model_relabel] }

  def readonly? = persisted?
end
