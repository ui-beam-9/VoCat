import { api } from "../api";

// 本地通讯录（服务端数据库存储）：把号码保存为联系人，读写在 /api/contacts。

export interface Contact {
  id: number;
  name: string;
  number: string;
  createdAt?: string;
  updatedAt?: string;
}

export async function fetchContacts(): Promise<Contact[]> {
  const result = await api<{ entries?: Contact[] }>("/contacts");
  return result.entries ?? [];
}

// 号码唯一：重复保存同一号码时更新姓名。
export async function saveContact(name: string, number: string): Promise<Contact> {
  const result = await api<{ entry: Contact }>("/contacts", {
    method: "POST",
    body: { name, number },
  });
  return result.entry;
}
