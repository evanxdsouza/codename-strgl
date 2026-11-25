class AddStruggleRoleToUsers < ActiveRecord::Migration[8.1]
  def change
    # Review permission for struggle tracker submissions: "reviewer" sees
    # normal review evidence, "fraud_squad" also sees escalated evidence.
    add_column :users, :struggle_role, :string, null: false, default: "none"
  end
end
