class CreateProjectCorrections < ActiveRecord::Migration[8.1]
  def change
    create_table :project_corrections do |t|
      t.references :user, null: false, foreign_key: true
      t.float :start_time, null: false
      t.float :end_time, null: false
      # NULL means every project in the range was moved.
      t.string :from_project
      t.string :to_project, null: false
      t.integer :heartbeats_count, null: false, default: 0
      t.timestamps
    end
    add_index :project_corrections, [ :user_id, :start_time ]
  end
end
