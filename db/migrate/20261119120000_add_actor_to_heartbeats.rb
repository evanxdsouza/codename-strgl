class AddActorToHeartbeats < ActiveRecord::Migration[8.1]
  def change
    # Who produced the activity: "human", "agent", or NULL for clients that do
    # not say (editor plugins, imports), which is counted as human.
    add_column :heartbeats, :actor, :string
  end
end
