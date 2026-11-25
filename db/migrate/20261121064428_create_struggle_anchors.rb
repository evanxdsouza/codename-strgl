class CreateStruggleAnchors < ActiveRecord::Migration[8.1]
  def change
    # A collector's evidence chain head, received without any content. Its
    # created_at bounds when the evidence before it existed on the device.
    create_table :struggle_anchors do |t|
      t.references :user, null: false, foreign_key: true
      t.string :device_id, null: false
      t.bigint :seq, null: false
      t.string :head, null: false
      t.timestamps
    end
    add_index :struggle_anchors, [ :user_id, :device_id, :seq ], unique: true
  end
end
