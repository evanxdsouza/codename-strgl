# A user's after-the-fact change of which project a time range belongs to.
# Applying one soft-deletes the matching visible heartbeats and inserts copies
# under the new project, so the original attribution stays in the database and
# this row records what moved. Splitting a session is reassigning part of it.
class ProjectCorrection < ApplicationRecord
  MAX_RANGE = 7.days

  belongs_to :user

  validates :start_time, :end_time, :to_project, presence: true
  validates :to_project, length: { maximum: 255 }, exclusion: { in: [ HeartbeatIngest::LAST_PROJECT_SENTINEL ] }
  validate :range_is_valid
  validate :changes_project

  # Heartbeats the correction moves: visible ones in [start_time, end_time),
  # optionally only those currently in from_project.
  def matching_heartbeats
    scope = user.heartbeats.where(time: start_time...end_time)
    scope = scope.where(project: from_project) if from_project
    scope.where("project IS DISTINCT FROM ?", to_project)
  end

  def apply!
    self.to_project = to_project.to_s.gsub(/[[:cntrl:]]/, "").strip
    self.from_project = from_project.presence
    validate!

    transaction do
      originals = matching_heartbeats.to_a
      self.heartbeats_count = originals.length
      save!
      next if originals.empty?

      now = Time.current
      # with_excluded: the ids are already limited to visible rows above.
      Heartbeat.with_excluded.where(id: originals.map(&:id)).update_all(deleted_at: now)
      copies = originals.map do |heartbeat|
        attributes = heartbeat.attributes.except("id", "deleted_at", "created_at", "updated_at")
          .merge("project" => to_project)
        attributes.merge("fields_hash" => Heartbeat.generate_fields_hash(attributes), "created_at" => now, "updated_at" => now)
      end
      Heartbeat.insert_all(copies, unique_by: :fields_hash)
    end
    DashboardRollupRefreshJob.schedule_for(user_id)
    AttemptProjectRepoMappingJob.enqueue_for(user, to_project)
    self
  end

  private

  def range_is_valid
    return if start_time.blank? || end_time.blank?

    errors.add(:end_time, "must be after the start") if end_time <= start_time
    errors.add(:end_time, "must be within #{MAX_RANGE.inspect} of the start") if end_time - start_time > MAX_RANGE
  end

  def changes_project
    errors.add(:to_project, "is the same as the original project") if from_project.present? && from_project == to_project
  end
end
