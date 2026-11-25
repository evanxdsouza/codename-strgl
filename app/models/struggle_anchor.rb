# A collector's evidence chain head at one sequence number, sent without any
# content. The server's receipt time bounds when that evidence existed.
class StruggleAnchor < ApplicationRecord
  belongs_to :user

  validates :device_id, presence: true, length: { maximum: 64 }
  validates :seq, numericality: { only_integer: true, greater_than: 0 }
  validates :head, format: { with: /\A[0-9a-f]{64}\z/ }
end
