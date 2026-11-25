# One turn a live submission claims. The partial unique index on
# (user_id, turn_id) keeps a turn from being credited by two submissions.
class StruggleTurnClaim < ApplicationRecord
  belongs_to :struggle_submission
  belongs_to :user
end
